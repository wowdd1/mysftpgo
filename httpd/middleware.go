package httpd

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/jwtauth/v5"
	"github.com/lestrrat-go/jwx/jwt"
	"github.com/rs/xid"
	"github.com/sftpgo/sdk"

	"github.com/drakkan/sftpgo/v2/common"
	"github.com/drakkan/sftpgo/v2/dataprovider"
	"github.com/drakkan/sftpgo/v2/logger"
	"github.com/drakkan/sftpgo/v2/util"
)

var (
	forwardedProtoKey = &contextKey{"forwarded proto"}
	errInvalidToken   = errors.New("invalid JWT token")
)

type contextKey struct {
	name string
}

func (k *contextKey) String() string {
	return "context value " + k.name
}

func validateJWTToken(w http.ResponseWriter, r *http.Request, audience tokenAudience) error {
	token, _, err := jwtauth.FromContext(r.Context())

	var redirectPath string
	if audience == tokenAudienceWebAdmin {
		redirectPath = webLoginPath
	} else {
		redirectPath = webClientLoginPath
	}

	isAPIToken := (audience == tokenAudienceAPI || audience == tokenAudienceAPIUser)

	if err != nil || token == nil {
		logger.Debug(logSender, "", "error getting jwt token: %v", err)
		if isAPIToken {
			sendAPIResponse(w, r, err, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		} else {
			http.Redirect(w, r, redirectPath, http.StatusFound)
		}
		return errInvalidToken
	}

	err = jwt.Validate(token)
	if err != nil {
		logger.Debug(logSender, "", "error validating jwt token: %v", err)
		if isAPIToken {
			sendAPIResponse(w, r, err, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		} else {
			http.Redirect(w, r, redirectPath, http.StatusFound)
		}
		return errInvalidToken
	}
	if isTokenInvalidated(r) {
		logger.Debug(logSender, "", "the token has been invalidated")
		if isAPIToken {
			sendAPIResponse(w, r, nil, "Your token is no longer valid", http.StatusUnauthorized)
		} else {
			http.Redirect(w, r, redirectPath, http.StatusFound)
		}
		return errInvalidToken
	}
	// a user with a partial token will be always redirected to the appropriate two factor auth page
	if err := checkPartialAuth(w, r, audience, token.Audience()); err != nil {
		return err
	}
	if !util.IsStringInSlice(audience, token.Audience()) {
		logger.Debug(logSender, "", "the token is not valid for audience %#v", audience)
		if isAPIToken {
			sendAPIResponse(w, r, nil, "Your token audience is not valid", http.StatusUnauthorized)
		} else {
			http.Redirect(w, r, redirectPath, http.StatusFound)
		}
		return errInvalidToken
	}
	return nil
}

func validateJWTPartialToken(w http.ResponseWriter, r *http.Request, audience tokenAudience) error {
	token, _, err := jwtauth.FromContext(r.Context())
	var notFoundFunc func(w http.ResponseWriter, r *http.Request, err error)
	if audience == tokenAudienceWebAdminPartial {
		notFoundFunc = renderNotFoundPage
	} else {
		notFoundFunc = renderClientNotFoundPage
	}
	if err != nil || token == nil || jwt.Validate(token) != nil {
		notFoundFunc(w, r, nil)
		return errInvalidToken
	}
	if isTokenInvalidated(r) {
		notFoundFunc(w, r, nil)
		return errInvalidToken
	}
	if !util.IsStringInSlice(audience, token.Audience()) {
		logger.Debug(logSender, "", "the token is not valid for audience %#v", audience)
		notFoundFunc(w, r, nil)
		return errInvalidToken
	}

	return nil
}

func jwtAuthenticatorPartial(audience tokenAudience) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := validateJWTPartialToken(w, r, audience); err != nil {
				return
			}

			// Token is authenticated, pass it through
			next.ServeHTTP(w, r)
		})
	}
}

func jwtAuthenticatorAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := validateJWTToken(w, r, tokenAudienceAPI); err != nil {
			return
		}

		// Token is authenticated, pass it through
		next.ServeHTTP(w, r)
	})
}

func jwtAuthenticatorAPIUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := validateJWTToken(w, r, tokenAudienceAPIUser); err != nil {
			return
		}

		// Token is authenticated, pass it through
		next.ServeHTTP(w, r)
	})
}

func jwtAuthenticatorWebAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := validateJWTToken(w, r, tokenAudienceWebAdmin); err != nil {
			return
		}

		// Token is authenticated, pass it through
		next.ServeHTTP(w, r)
	})
}

func jwtAuthenticatorWebClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := validateJWTToken(w, r, tokenAudienceWebClient); err != nil {
			return
		}

		// Token is authenticated, pass it through
		next.ServeHTTP(w, r)
	})
}

func checkHTTPUserPerm(perm string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, claims, err := jwtauth.FromContext(r.Context())
			if err != nil {
				if isWebRequest(r) {
					renderClientBadRequestPage(w, r, err)
				} else {
					sendAPIResponse(w, r, err, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
				}
				return
			}
			tokenClaims := jwtTokenClaims{}
			tokenClaims.Decode(claims)
			// for web client perms are negated and not granted
			if tokenClaims.hasPerm(perm) {
				if isWebRequest(r) {
					renderClientForbiddenPage(w, r, "You don't have permission for this action")
				} else {
					sendAPIResponse(w, r, nil, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func checkPerm(perm string) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, claims, err := jwtauth.FromContext(r.Context())
			if err != nil {
				if isWebRequest(r) {
					renderBadRequestPage(w, r, err)
				} else {
					sendAPIResponse(w, r, err, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
				}
				return
			}
			tokenClaims := jwtTokenClaims{}
			tokenClaims.Decode(claims)

			if !tokenClaims.hasPerm(perm) {
				if isWebRequest(r) {
					renderForbiddenPage(w, r, "You don't have permission for this action")
				} else {
					sendAPIResponse(w, r, nil, http.StatusText(http.StatusForbidden), http.StatusForbidden)
				}
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func verifyCSRFHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenString := r.Header.Get(csrfHeaderToken)
		token, err := jwtauth.VerifyToken(csrfTokenAuth, tokenString)
		if err != nil || token == nil {
			logger.Debug(logSender, "", "error validating CSRF header: %v", err)
			sendAPIResponse(w, r, err, "Invalid token", http.StatusForbidden)
			return
		}

		if !util.IsStringInSlice(tokenAudienceCSRF, token.Audience()) {
			logger.Debug(logSender, "", "error validating CSRF header audience")
			sendAPIResponse(w, r, errors.New("the token is not valid"), "", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func checkAPIKeyAuth(tokenAuth *jwtauth.JWTAuth, scope dataprovider.APIKeyScope) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-SFTPGO-API-KEY")
			if apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}
			keyParams := strings.SplitN(apiKey, ".", 3)
			if len(keyParams) < 2 {
				logger.Debug(logSender, "", "invalid api key %#v", apiKey)
				sendAPIResponse(w, r, errors.New("the provided api key is not valid"), "", http.StatusBadRequest)
				return
			}
			keyID := keyParams[0]
			key := keyParams[1]
			apiUser := ""
			if len(keyParams) > 2 {
				apiUser = keyParams[2]
			}

			k, err := dataprovider.APIKeyExists(keyID)
			if err != nil {
				logger.Debug(logSender, "invalid api key %#v: %v", apiKey, err)
				sendAPIResponse(w, r, errors.New("the provided api key is not valid"), "", http.StatusBadRequest)
				return
			}
			if err := k.Authenticate(key); err != nil {
				logger.Debug(logSender, "unable to authenticate api key %#v: %v", apiKey, err)
				sendAPIResponse(w, r, fmt.Errorf("the provided api key cannot be authenticated"), "", http.StatusUnauthorized)
				return
			}
			if scope == dataprovider.APIKeyScopeAdmin {
				if k.Admin != "" {
					apiUser = k.Admin
				}
				if err := authenticateAdminWithAPIKey(apiUser, keyID, tokenAuth, r); err != nil {
					logger.Debug(logSender, "", "unable to authenticate admin %#v associated with api key %#v: %v",
						apiUser, apiKey, err)
					sendAPIResponse(w, r, fmt.Errorf("the admin associated with the provided api key cannot be authenticated"),
						"", http.StatusUnauthorized)
					return
				}
			} else {
				if k.User != "" {
					apiUser = k.User
				}
				if err := authenticateUserWithAPIKey(apiUser, keyID, tokenAuth, r); err != nil {
					logger.Debug(logSender, "", "unable to authenticate user %#v associated with api key %#v: %v",
						apiUser, apiKey, err)
					code := http.StatusUnauthorized
					if errors.Is(err, common.ErrInternalFailure) {
						code = http.StatusInternalServerError
					}
					sendAPIResponse(w, r, errors.New("the user associated with the provided api key cannot be authenticated"),
						"", code)
					return
				}
			}
			dataprovider.UpdateAPIKeyLastUse(&k) //nolint:errcheck

			next.ServeHTTP(w, r)
		})
	}
}

func forbidAPIKeyAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := getTokenClaims(r)
		if err != nil || claims.Username == "" {
			sendAPIResponse(w, r, err, "Invalid token claims", http.StatusBadRequest)
			return
		}
		if claims.APIKeyID != "" {
			sendAPIResponse(w, r, nil, "API key authentication is not allowed", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func authenticateAdminWithAPIKey(username, keyID string, tokenAuth *jwtauth.JWTAuth, r *http.Request) error {
	if username == "" {
		return errors.New("the provided key is not associated with any admin and no username was provided")
	}
	admin, err := dataprovider.AdminExists(username)
	if err != nil {
		return err
	}
	if !admin.Filters.AllowAPIKeyAuth {
		return fmt.Errorf("API key authentication disabled for admin %#v", admin.Username)
	}
	if err := admin.CanLogin(util.GetIPFromRemoteAddress(r.RemoteAddr)); err != nil {
		return err
	}
	c := jwtTokenClaims{
		Username:    admin.Username,
		Permissions: admin.Permissions,
		Signature:   admin.GetSignature(),
		APIKeyID:    keyID,
	}

	resp, err := c.createTokenResponse(tokenAuth, tokenAudienceAPI)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", fmt.Sprintf("Bearer %v", resp["access_token"]))
	dataprovider.UpdateAdminLastLogin(&admin)
	return nil
}

func authenticateUserWithAPIKey(username, keyID string, tokenAuth *jwtauth.JWTAuth, r *http.Request) error {
	ipAddr := util.GetIPFromRemoteAddress(r.RemoteAddr)
	if username == "" {
		err := errors.New("the provided key is not associated with any user and no username was provided")
		updateLoginMetrics(&dataprovider.User{BaseUser: sdk.BaseUser{Username: username}}, ipAddr, err)
		return err
	}
	if err := common.Config.ExecutePostConnectHook(ipAddr, common.ProtocolHTTP); err != nil {
		return err
	}
	user, err := dataprovider.UserExists(username)
	if err != nil {
		updateLoginMetrics(&dataprovider.User{BaseUser: sdk.BaseUser{Username: username}}, ipAddr, err)
		return err
	}
	if !user.Filters.AllowAPIKeyAuth {
		err := fmt.Errorf("API key authentication disabled for user %#v", user.Username)
		updateLoginMetrics(&user, ipAddr, err)
		return err
	}
	if err := user.CheckLoginConditions(); err != nil {
		updateLoginMetrics(&user, ipAddr, err)
		return err
	}
	connectionID := fmt.Sprintf("%v_%v", common.ProtocolHTTP, xid.New().String())
	if err := checkHTTPClientUser(&user, r, connectionID); err != nil {
		updateLoginMetrics(&user, ipAddr, err)
		return err
	}
	lastLogin := util.GetTimeFromMsecSinceEpoch(user.LastLogin)
	diff := -time.Until(lastLogin)
	if diff < 0 || diff > 10*time.Minute {
		defer user.CloseFs() //nolint:errcheck
		err = user.CheckFsRoot(connectionID)
		if err != nil {
			updateLoginMetrics(&user, ipAddr, common.ErrInternalFailure)
			return common.ErrInternalFailure
		}
	}
	c := jwtTokenClaims{
		Username:    user.Username,
		Permissions: user.Filters.WebClient,
		Signature:   user.GetSignature(),
		APIKeyID:    keyID,
	}

	resp, err := c.createTokenResponse(tokenAuth, tokenAudienceAPIUser)
	if err != nil {
		updateLoginMetrics(&user, ipAddr, common.ErrInternalFailure)
		return err
	}
	r.Header.Set("Authorization", fmt.Sprintf("Bearer %v", resp["access_token"]))
	dataprovider.UpdateLastLogin(&user)
	updateLoginMetrics(&user, ipAddr, nil)

	return nil
}

func checkPartialAuth(w http.ResponseWriter, r *http.Request, audience string, tokenAudience []string) error {
	if audience == tokenAudienceWebAdmin && util.IsStringInSlice(tokenAudienceWebAdminPartial, tokenAudience) {
		http.Redirect(w, r, webAdminTwoFactorPath, http.StatusFound)
		return errInvalidToken
	}
	if audience == tokenAudienceWebClient && util.IsStringInSlice(tokenAudienceWebClientPartial, tokenAudience) {
		http.Redirect(w, r, webClientTwoFactorPath, http.StatusFound)
		return errInvalidToken
	}
	return nil
}
