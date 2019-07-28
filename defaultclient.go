/*
 * Copyright 2018 AccelByte Inc
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package iam

import (
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/AccelByte/bloom"
	"github.com/patrickmn/go-cache"
)

// JFlags constants
const (
	UserStatusEmailVerified = 1
	UserStatusPhoneVerified = 1 << 1
	UserStatusAnonymous     = 1 << 2
)

const (
	jwksPath              = "/oauth/jwks"
	grantPath             = "/oauth/token"
	revocationListPath    = "/oauth/revocationlist"
	verifyPath            = "/oauth/verify"
	getRolePath           = "/roles"
	clientInformationPath = "/v3/admin/namespaces/%s/clients/%s"

	defaultTokenRefreshRate              = 0.8
	maxBackOffTime                       = 65 * time.Second
	defaultRoleCacheTime                 = 60 * time.Second
	defaultJWKSRefreshInterval           = 60 * time.Second
	defaultRevocationListRefreshInterval = 60 * time.Second

	baseURIKey             = "baseURI"
	baseURICacheExpiration = 1 * time.Minute
	scopeSeparator         = " "
)

// Config contains IAM configurations
type Config struct {
	BaseURL                       string
	ClientID                      string
	ClientSecret                  string
	RolesCacheExpirationTime      time.Duration // default: 60s
	JWKSRefreshInterval           time.Duration // default: 60s
	RevocationListRefreshInterval time.Duration // default: 60s
}

// DefaultClient define oauth client config
type DefaultClient struct {
	keys                       map[string]*rsa.PublicKey
	clientAccessToken          string
	config                     *Config
	rolePermissionCache        *cache.Cache
	revocationFilter           *bloom.Filter
	revokedUsers               map[string]time.Time
	tokenRefreshActive         bool
	localValidationActive      bool
	jwksRefreshError           error
	revocationListRefreshError error
	tokenRefreshError          error
	remoteTokenValidation      func(accessToken string) (bool, error)
	baseURICache               *cache.Cache
	// for easily mocking the HTTP call
	httpClient HTTPClient
}

// HTTPClient is an interface for http.Client. The purpose for having this so we could easily mock the HTTP call.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// NewDefaultClient creates new IAM DefaultClient
func NewDefaultClient(config *Config) Client {
	if config.RolesCacheExpirationTime <= 0 {
		config.RolesCacheExpirationTime = defaultRoleCacheTime
	}
	if config.JWKSRefreshInterval <= 0 {
		config.JWKSRefreshInterval = defaultJWKSRefreshInterval
	}
	if config.RevocationListRefreshInterval <= 0 {
		config.RevocationListRefreshInterval = defaultRevocationListRefreshInterval
	}

	client := &DefaultClient{
		config:              config,
		rolePermissionCache: cache.New(config.RolesCacheExpirationTime, 2*config.RolesCacheExpirationTime),
	}
	client.remoteTokenValidation = client.validateAccessToken

	client.baseURICache = cache.New(baseURICacheExpiration, baseURICacheExpiration)
	client.httpClient = &http.Client{}

	return client
}

// ClientTokenGrant starts client token grant to get client bearer token for role caching
func (client *DefaultClient) ClientTokenGrant() error {
	refreshInterval, err := client.clientTokenGrant()
	if err != nil {
		return err
	}
	go func() {
		client.tokenRefreshActive = true
		time.Sleep(refreshInterval)
		client.refreshAccessToken()
	}()
	return nil
}

// ClientToken returns client access token
func (client *DefaultClient) ClientToken() string {
	return client.clientAccessToken
}

// StartLocalValidation starts goroutines to refresh JWK and revocation list periodically
// this enables local token validation
func (client *DefaultClient) StartLocalValidation() error {
	err := client.getJWKS()
	if err != nil {
		return fmt.Errorf("unable to get JWKS: %v", err)
	}

	err = client.getRevocationList()
	if err != nil {
		return fmt.Errorf("unable to get revocation list: %v", err)
	}

	go client.refreshJWKS()
	go client.refreshRevocationList()

	client.localValidationActive = true
	return nil
}

// ValidateAccessToken validates access token by calling IAM service
func (client *DefaultClient) ValidateAccessToken(accessToken string) (bool, error) {
	return client.remoteTokenValidation(accessToken)
}

// ValidateAndParseClaims validates access token locally and returns the JWT claims contained in the token
func (client *DefaultClient) ValidateAndParseClaims(accessToken string) (*JWTClaims, error) {
	if !client.localValidationActive {
		return nil, errors.New("local validation is not active, activate by calling StartLocalValidation()")
	}

	claims, err := client.validateJWT(accessToken)
	if err != nil {
		return nil, fmt.Errorf("unable to verify JWT : %v", err)
	}
	if client.userRevoked(claims.Subject, int64(claims.IssuedAt)) {
		return nil, errors.New("user has been revoked")
	}
	if client.tokenRevoked(accessToken) {
		return nil, errors.New("token has been revoked")
	}

	return claims, nil
}

// ValidatePermission validates if an access token has right for a specific permission
// requiredPermission: permission to access resource, example:
// 		{Resource: "NAMESPACE:{namespace}:USER:{userId}", Action: 2}
// permissionResources: resource string to replace the `{}` placeholder in
// 		`requiredPermission`, example: p["{namespace}"] = "accelbyte"
func (client *DefaultClient) ValidatePermission(claims *JWTClaims,
	requiredPermission Permission, permissionResources map[string]string) (bool, error) {
	if claims == nil {
		return false, nil
	}
	for placeholder, value := range permissionResources {
		requiredPermission.Resource = strings.Replace(requiredPermission.Resource, placeholder, value, 1)
	}
	if client.permissionAllowed(claims.Permissions, requiredPermission) {
		return true, nil
	}
	for _, roleID := range claims.Roles {
		grantedRolePermissions, err := client.getRolePermission(roleID)
		if err != nil {
			if err == errRoleNotFound {
				continue
			}
			return false, err
		}
		grantedRolePermissions = client.applyUserPermissionResourceValues(grantedRolePermissions, claims)
		if client.permissionAllowed(grantedRolePermissions, requiredPermission) {
			return true, nil
		}
	}
	return false, nil
}

// ValidateRole validates if an access token has a specific role
func (client *DefaultClient) ValidateRole(requiredRoleID string, claims *JWTClaims) (bool, error) {
	for _, grantedRoleID := range claims.Roles {
		if grantedRoleID == requiredRoleID {
			return true, nil
		}
	}
	return false, nil
}

// UserPhoneVerificationStatus gets user phone verification status on access token
func (client *DefaultClient) UserPhoneVerificationStatus(claims *JWTClaims) (bool, error) {
	phoneVerified := claims.JusticeFlags&UserStatusPhoneVerified == UserStatusPhoneVerified
	return phoneVerified, nil
}

// UserEmailVerificationStatus gets user email verification status on access token
func (client *DefaultClient) UserEmailVerificationStatus(claims *JWTClaims) (bool, error) {
	emailVerified := claims.JusticeFlags&UserStatusEmailVerified == UserStatusEmailVerified
	return emailVerified, nil
}

// UserAnonymousStatus gets user anonymous status on access token
func (client *DefaultClient) UserAnonymousStatus(claims *JWTClaims) (bool, error) {
	anonymousStatus := claims.JusticeFlags&UserStatusAnonymous == UserStatusAnonymous
	return anonymousStatus, nil
}

// HasBan validates if certain ban exist
func (client *DefaultClient) HasBan(claims *JWTClaims, banType string) bool {
	for _, ban := range claims.Bans {
		if ban.Ban == banType {
			return true
		}
	}
	return false
}

// HealthCheck lets caller know the health of the IAM client
func (client *DefaultClient) HealthCheck() bool {
	if client.jwksRefreshError != nil {
		return false
	}
	if client.revocationListRefreshError != nil {
		return false
	}
	if client.tokenRefreshActive && client.tokenRefreshError != nil {
		return false
	}
	return true
}

// ValidateAudience validate audience of user access token
func (client *DefaultClient) ValidateAudience(claims *JWTClaims) error {
	if claims == nil {
		return errors.New("claims is nil")
	}
	// no need to check if no audience found in the claims. https://tools.ietf.org/html/rfc7519#section-4.1.3
	if claims.Audience == nil {
		fmt.Printf("[IAM-Go-SDK] No audience found in the token. Skipping the audience validation\n")
		return nil
	}
	baseURI, found := client.baseURICache.Get(baseURIKey)
	if !found {
		path := fmt.Sprintf(clientInformationPath, claims.Namespace, client.config.ClientID)
		getClientInformationURL := client.config.BaseURL + path
		err := client.getClientInformation(getClientInformationURL)
		if err != nil {
			fmt.Printf("[IAM-Go-SDK] get client detail returns error: %v\n", err)
			return err
		}
		baseURI, _ = client.baseURICache.Get(baseURIKey)
	}

	isAllowed := false
	for _, reqAud := range claims.Audience {
		if reqAud == baseURI {
			isAllowed = true
			break
		}
	}

	if !isAllowed {
		return errors.New("audience doesn't match the client's base uri. access denied")
	}

	return nil
}

// ValidateScope validate scope of user access token
func (client *DefaultClient) ValidateScope(claims *JWTClaims, reqScope string) error {
	scopes := strings.Split(claims.Scope, scopeSeparator)

	var isValid = false
	for _, scope := range scopes {
		if reqScope == scope {
			isValid = true
			break
		}
	}

	if !isValid {
		return errors.New("insufficient scope")
	}

	return nil
}

// getClientInformation get client base URI
// need client access token for authorization
func (client *DefaultClient) getClientInformation(getClientInformationURL string) (err error) {

	clientInformation := struct {
		BaseURI string `json:"BaseUri"`
	}{}

	req, err := http.NewRequest(http.MethodGet, getClientInformationURL, nil)
	if err != nil {
		return fmt.Errorf("unable to create new http request: %v", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("Authorization", "Bearer "+client.clientAccessToken)
	resp, err := client.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("unable to do http request: %v", err)
	}

	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("unable to read body response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unable to get client information : error code : %d, error message : %s",
			resp.StatusCode, string(bodyBytes))
	}

	err = json.Unmarshal(bodyBytes, &clientInformation)
	if err != nil {
		return fmt.Errorf("unable to unmarshal body: %v", err)
	}

	client.baseURICache.Set(baseURIKey, clientInformation.BaseURI, cache.DefaultExpiration)

	return nil
}
