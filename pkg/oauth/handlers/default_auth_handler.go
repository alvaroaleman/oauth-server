package handlers

import (
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/endpoints/request"

	oauthapi "github.com/openshift/api/oauth/v1"
	authapi "github.com/openshift/oauth-server/pkg/api"
)

// unionAuthenticationHandler is an oauth.AuthenticationHandler that muxes multiple challenge handlers and redirect handlers
type unionAuthenticationHandler struct {
	challengers      map[string]AuthenticationChallenger
	redirectors      *AuthenticationRedirectors
	errorHandler     AuthenticationErrorHandler
	selectionHandler AuthenticationSelectionHandler
}

// NewUnionAuthenticationHandler returns an oauth.AuthenticationHandler that muxes multiple challenge handlers and redirect handlers
func NewUnionAuthenticationHandler(passedChallengers map[string]AuthenticationChallenger, passedRedirectors *AuthenticationRedirectors, errorHandler AuthenticationErrorHandler, selectionHandler AuthenticationSelectionHandler) AuthenticationHandler {
	challengers := passedChallengers
	if challengers == nil {
		challengers = make(map[string]AuthenticationChallenger, 1)
	}

	redirectors := passedRedirectors
	if redirectors == nil {
		redirectors = new(AuthenticationRedirectors)
	}

	return &unionAuthenticationHandler{challengers: challengers, redirectors: redirectors, errorHandler: errorHandler, selectionHandler: selectionHandler}
}

const (
	// WarningHeaderMiscCode is the code for "Miscellaneous warning", which may be displayed to human users
	WarningHeaderMiscCode = "199"
	// WarningHeaderOpenShiftSource is the name of the agent adding the warning header
	WarningHeaderOpenShiftSource = "Origin"

	// There were more indexes, but were unused.
	warningHeaderCodeIndex = 1
	warningHeaderTextIndex = 3

	useRedirectParam = "idp"
)

var (
	// http://tools.ietf.org/html/rfc2616#section-14.46
	warningRegex = regexp.MustCompile(strings.Join([]string{
		// Beginning of the string
		`^`,
		// Exactly 3 digits (captured in group 1)
		`([0-9]{3})`,
		// A single space
		` `,
		// 1+ non-space characters (captured in group 2)
		`([^ ]+)`,
		// A single space
		` `,
		// quoted-string (value inside quotes is captured in group 3)
		`"((?:[^"\\]|\\.)*)"`,
		// Optionally followed by quoted HTTP-Date
		`(?: "([^"]+)")?`,
		// End of the string
		`$`,
	}, ""))
)

// AuthenticationNeeded looks at the oauth Client to determine whether it wants try to authenticate with challenges or using a redirect path
// If the client wants a challenge path, it muxes together all the different challenges from the challenge handlers
// If (the client wants a redirect path) and ((there is one redirect handler) or (a redirect handler was requested via the "idp" parameter),
// then the redirect handler is called.  Otherwise, you get an error (currently) or a redirect to a page letting you choose how you'd like to authenticate.
// It returns whether the response was written and/or an error
func (authHandler *unionAuthenticationHandler) AuthenticationNeeded(apiClient authapi.Client, w http.ResponseWriter, req *http.Request) (bool, error) {
	client, ok := apiClient.GetUserData().(*oauthapi.OAuthClient)
	if !ok {
		return false, fmt.Errorf("apiClient data was not an oauthapi.OAuthClient")
	}

	if client.RespondWithChallenges {
		errors := []error{}
		headers := http.Header(make(map[string][]string))
		for _, challengingHandler := range authHandler.challengers {
			currHeaders, err := challengingHandler.AuthenticationChallenge(req)
			if err != nil {
				errors = append(errors, err)
				continue
			}

			// merge header values
			mergeHeaders(headers, currHeaders)
		}

		if len(headers) > 0 {
			mergeHeaders(w.Header(), headers)

			redirectHeader := w.Header().Get("Location")
			redirectHeaders := w.Header()[http.CanonicalHeaderKey("Location")]
			challengeHeader := w.Header().Get("WWW-Authenticate")
			switch {
			case len(redirectHeader) > 0 && len(challengeHeader) > 0:
				errors = append(errors, fmt.Errorf("redirect header (Location: %s) and challenge header (WWW-Authenticate: %s) cannot both be set", redirectHeader, challengeHeader))
				return false, kerrors.NewAggregate(errors)
			case len(redirectHeaders) > 1:
				errors = append(errors, fmt.Errorf("cannot set multiple redirect headers: %s", strings.Join(redirectHeaders, ", ")))
				return false, kerrors.NewAggregate(errors)
			case len(redirectHeader) > 0:
				w.WriteHeader(http.StatusFound)
			default:
				w.WriteHeader(http.StatusUnauthorized)
				ev := request.AuditEventFrom(req.Context())
				if ev != nil {
					// this code mimics the bits from k8s.io/apiserver/pkg/endpoints/filters/authn_audit.go
					// but since we don't accept failedHander here we need to manually alter the audit
					// event with information about failed authentication
					ev.ResponseStatus.Message = getAuthMethods(req)
				}
			}

			// Print Misc Warning headers (code 199) to the body
			if warnings, hasWarnings := w.Header()[http.CanonicalHeaderKey("Warning")]; hasWarnings {
				for _, warning := range warnings {
					warningParts := warningRegex.FindStringSubmatch(warning)
					if len(warningParts) != 0 && warningParts[warningHeaderCodeIndex] == WarningHeaderMiscCode {
						fmt.Fprintln(w, warningParts[warningHeaderTextIndex])
					}
				}
			}

			return true, nil

		}
		return false, kerrors.NewAggregate(errors)

	}

	// See if a single provider was selected
	redirectHandlerName := req.URL.Query().Get(useRedirectParam)
	if len(redirectHandlerName) > 0 {
		redirectHandler, ok := authHandler.redirectors.Get(redirectHandlerName)
		if !ok {
			return false, fmt.Errorf("Unable to locate redirect handler: %v", html.EscapeString(redirectHandlerName))
		}
		err := redirectHandler.AuthenticationRedirect(w, req)
		if err != nil {
			return authHandler.errorHandler.AuthenticationError(err, w, req)
		}
		return true, nil
	}

	// Delegate to provider selection
	if authHandler.selectionHandler != nil {
		providers := []authapi.ProviderInfo{}
		for _, name := range authHandler.redirectors.GetNames() {
			u := *req.URL
			q := u.Query()
			q.Set(useRedirectParam, name)
			u.RawQuery = q.Encode()
			providerInfo := authapi.ProviderInfo{
				Name: name,
				URL:  u.String(),
			}
			providers = append(providers, providerInfo)
		}
		selectedProvider, handled, err := authHandler.selectionHandler.SelectAuthentication(providers, w, req)
		if err != nil {
			return authHandler.errorHandler.AuthenticationError(err, w, req)
		}
		if handled {
			return handled, nil
		}
		if selectedProvider != nil {
			redirectHandler, ok := authHandler.redirectors.Get(selectedProvider.Name)
			if !ok {
				return false, fmt.Errorf("Unable to locate redirect handler: %v", selectedProvider.Name)
			}
			err := redirectHandler.AuthenticationRedirect(w, req)
			if err != nil {
				return authHandler.errorHandler.AuthenticationError(err, w, req)
			}
			return true, nil

		}
	}

	// Otherwise, automatically select a single provider, and error on multiple
	if authHandler.redirectors.Count() == 1 {
		redirectHandler, ok := authHandler.redirectors.Get(authHandler.redirectors.GetNames()[0])
		if !ok {
			return authHandler.errorHandler.AuthenticationError(fmt.Errorf("No valid redirectors"), w, req)
		}

		err := redirectHandler.AuthenticationRedirect(w, req)
		if err != nil {
			return authHandler.errorHandler.AuthenticationError(err, w, req)
		}
		return true, nil

	} else if authHandler.redirectors.Count() > 1 {
		// TODO this clearly doesn't work right.  There should probably be a redirect to an interstitial page.
		// however, this is just as good as we have now.
		return false, fmt.Errorf("Too many potential redirect handlers: %v", authHandler.redirectors)
	}

	return false, nil
}

func mergeHeaders(dest http.Header, toAdd http.Header) {
	for key, values := range toAdd {
		for _, value := range values {
			dest.Add(key, value)
		}
	}
}

// getAuthMethods is copied from k8s.io/apiserver/pkg/endpoints/filters/authn_audit.go
// to be able to return information about failed authentication
func getAuthMethods(req *http.Request) string {
	authMethods := []string{}

	if _, _, ok := req.BasicAuth(); ok {
		authMethods = append(authMethods, "basic")
	}

	auth := strings.TrimSpace(req.Header.Get("Authorization"))
	parts := strings.Split(auth, " ")
	if len(parts) > 1 && strings.ToLower(parts[0]) == "bearer" {
		authMethods = append(authMethods, "bearer")
	}

	token := strings.TrimSpace(req.URL.Query().Get("access_token"))
	if len(token) > 0 {
		authMethods = append(authMethods, "access_token")
	}

	if req.TLS != nil && len(req.TLS.PeerCertificates) > 0 {
		authMethods = append(authMethods, "x509")
	}

	if len(authMethods) > 0 {
		return fmt.Sprintf("Authentication failed, attempted: %s", strings.Join(authMethods, ", "))
	}
	return "Authentication failed, no credentials provided"
}
