/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/utils"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/gravitational/trace"
	"google.golang.org/grpc/peer"
)

const (
	expectedSTSIdentityRequestBody = "Action=GetCallerIdentity&Version=2011-06-15"
	stsHost                        = "sts.amazonaws.com"
	challengeHeaderKey             = "X-Teleport-Challenge"
	normalizedChallengeHeaderKey   = "x-teleport-challenge"
	authHeaderKey                  = "Authorization"
)

var signedHeadersRe = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=\S+, SignedHeaders=(\S+), Signature=\S+$`)

func validateSTSIdentityRequest(req *http.Request, challenge string) error {
	if req.Host != stsHost {
		return trace.AccessDenied("sts identity request is for unknown host %q", req.Host)
	}

	if req.Method != http.MethodPost {
		return trace.AccessDenied("sts identity request method %q does not match expected method %q", req.RequestURI, http.MethodPost)
	}

	if req.Header.Get(challengeHeaderKey) != challenge {
		return trace.AccessDenied("sts identity request does not include challenge header or it does not match")
	}

	authHeader := req.Header.Get(authHeaderKey)
	matches := signedHeadersRe.FindStringSubmatch(authHeader)
	// first match should be the full header, second is the SignedHeaders
	if len(matches) < 2 {
		return trace.AccessDenied("sts identity request Authorization header is invalid")
	}
	signedHeadersString := string(matches[1])
	signedHeaders := strings.Split(signedHeadersString, ";")
	if !utils.SliceContainsStr(signedHeaders, normalizedChallengeHeaderKey) {
		return trace.AccessDenied("sts identity request auth header %q does not include "+
			normalizedChallengeHeaderKey+" as a signed header", authHeader)
	}

	// read and replace request body
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return trace.Wrap(err)
	}
	req.Body = io.NopCloser(bytes.NewBuffer(body))

	if !bytes.Equal([]byte(expectedSTSIdentityRequestBody), body) {
		return trace.BadParameter("sts request body %q does not equal expected %q", string(body), expectedSTSIdentityRequestBody)
	}

	return nil
}

func parseSTSRequest(req []byte) (*http.Request, error) {
	httpReq, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(req)))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Unset RequestURI and set req.URL instead (necessary quirk of sending a
	// request parsed by http.ReadRequest). Also, force https here.
	httpReq.RequestURI = ""
	httpReq.URL = &url.URL{
		Scheme: "https",
		Host:   stsHost,
	}
	return httpReq, nil
}

type stsIdentityResponse struct {
	GetCallerIdentityResponse struct {
		GetCallerIdentityResult awsIdentity
	}
}

type awsIdentity struct {
	Account string
	Arn     string
}

type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

func executeSTSIdentityRequest(ctx context.Context, client httpClient, req *http.Request) (identity awsIdentity, err error) {
	req = req.WithContext(ctx)
	resp, err := client.Do(req)
	if err != nil {
		return identity, trace.Wrap(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return identity, trace.Wrap(err)
	}

	if resp.StatusCode != http.StatusOK {
		return identity, trace.AccessDenied("aws sts api returned status: %q body: %q",
			resp.Status, body)
	}

	var identityResponse stsIdentityResponse
	if err := json.Unmarshal(body, &identityResponse); err != nil {
		return identity, trace.Wrap(err)
	}
	return identityResponse.GetCallerIdentityResponse.GetCallerIdentityResult, nil
}

// arnMatches returns true if arn matches the pattern. pattern should be an AWS
// ARN which may include "*" to match any combination of zero or more characters
// and "?" to match any single character, see https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_resource.html
func arnMatches(pattern, arn string) (bool, error) {
	// asterisk should match zero or
	pattern = strings.ReplaceAll(pattern, "*", ".*")
	pattern = strings.ReplaceAll(pattern, "?", ".")
	pattern = "^" + pattern + "$"
	matched, err := regexp.MatchString(pattern, arn)
	return matched, trace.Wrap(err)
}

func checkIAMAllowRules(identity awsIdentity, provisionToken types.ProvisionToken) error {
	allowRules := provisionToken.GetAllowRules()
	for _, rule := range allowRules {
		// If this rule specifies an AWS account, the identity must match
		if len(rule.AWSAccount) > 0 {
			if rule.AWSAccount != identity.Account {
				// account doesn't match, continue to check the next rule
				continue
			}
		}
		if len(rule.AWSARN) > 0 {
			matches, err := arnMatches(rule.AWSARN, identity.Arn)
			if err != nil {
				return trace.Wrap(err)
			}
			if !matches {
				// arn doesn't match, continue to check the next rule
				continue
			}
		}
		// identity matches this allow rule
		return nil
	}
	return trace.AccessDenied("instance did not match any allow rules")
}

func (a *Server) checkIAMRequest(ctx context.Context, client httpClient, challenge string, req *types.RegisterUsingTokenRequest) error {
	tokenName := req.Token
	provisionToken, err := a.GetCache().GetToken(ctx, tokenName)
	if err != nil {
		return trace.Wrap(err)
	}
	if provisionToken.GetJoinMethod() != types.JoinMethodIAM {
		return trace.AccessDenied("this token does not support the IAM join method")
	}

	identityRequest, err := parseSTSRequest(req.STSIdentityRequest)
	if err != nil {
		return trace.Wrap(err)
	}

	if err := validateSTSIdentityRequest(identityRequest, challenge); err != nil {
		return trace.Wrap(err)
	}

	identity, err := executeSTSIdentityRequest(ctx, client, identityRequest)
	if err != nil {
		return trace.Wrap(err)
	}

	if err := checkIAMAllowRules(identity, provisionToken); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

type registerUsingIAMMethodServer interface {
	Send(*proto.RegisterUsingIAMMethodResponse) error
	Recv() (*types.RegisterUsingTokenRequest, error)
	Context() context.Context
}

func (a *Server) registerUsingIAMMethod(srv registerUsingIAMMethodServer) error {
	ctx := srv.Context()

	p, ok := peer.FromContext(ctx)
	if !ok {
		return trace.AccessDenied("failed to read peer information from gRPC context")
	}
	nodeAddress := p.Addr.String()

	// read 32 crypto-random bytes to generate the challenge
	challengeRawBytes := make([]byte, 32)
	if _, err := rand.Read(challengeRawBytes); err != nil {
		return trace.Wrap(err)
	}

	// encode the challenge to base64 so it can be sent in an HTTP header
	encoding := base64.RawStdEncoding
	challengeBase64 := make([]byte, encoding.EncodedLen(len(challengeRawBytes)))
	encoding.Encode(challengeBase64, challengeRawBytes)
	challengeString := string(challengeBase64)

	// send the challenge to the node
	if err := srv.Send(&proto.RegisterUsingIAMMethodResponse{
		Challenge: challengeString,
	}); err != nil {
		return trace.Wrap(err)
	}

	req, err := srv.Recv()
	if err != nil {
		return trace.Wrap(err)
	}

	req.RemoteAddr = nodeAddress

	if err := a.checkIAMRequest(ctx, http.DefaultClient, challengeString, req); err != nil {
		return trace.Wrap(err)
	}

	certs, err := a.RegisterUsingToken(*req)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(srv.Send(&proto.RegisterUsingIAMMethodResponse{
		Certs: certs,
	}))
}

// createSignedSTSIdentityRequest is called by the client and returns an
// sts:GetCallerIdentity request signed with the local AWS credentials
func createSignedSTSIdentityRequest(challenge string) ([]byte, error) {
	// use the aws sdk to generate the request
	sess := session.New()
	stsService := sts.New(sess)
	req, _ := stsService.GetCallerIdentityRequest(&sts.GetCallerIdentityInput{})
	// set challenge header
	req.HTTPRequest.Header.Set("X-Teleport-Challenge", challenge)
	// request json for simpler parsing
	req.HTTPRequest.Header.Set("accept", "application/json")
	// sign the request, including headers
	if err := req.Sign(); err != nil {
		return nil, trace.Wrap(err)
	}
	// write the signed HTTP request to a buffer
	var signedRequest bytes.Buffer
	if err := req.HTTPRequest.Write(&signedRequest); err != nil {
		return nil, trace.Wrap(err)
	}
	return signedRequest.Bytes(), nil
}
