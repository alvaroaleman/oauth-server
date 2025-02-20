package external

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/RangelReale/osincli"
	"k8s.io/klog/v2"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"

	"github.com/openshift/oauth-server/pkg/api"
	authapi "github.com/openshift/oauth-server/pkg/api"
	"github.com/openshift/oauth-server/pkg/audit"
	openshiftauthenticator "github.com/openshift/oauth-server/pkg/authenticator"
	"github.com/openshift/oauth-server/pkg/authenticator/identitymapper"
	"github.com/openshift/oauth-server/pkg/oauth/handlers"
	"github.com/openshift/oauth-server/pkg/server/csrf"
)

// Handler exposes an external oauth provider flow (including the call back) as an oauth.handlers.AuthenticationHandler to allow our internal oauth
// server to use an external oauth provider for authentication
type Handler struct {
	provider     Provider
	state        State
	clientConfig *osincli.ClientConfig
	client       *osincli.Client
	success      handlers.AuthenticationSuccessHandler
	errorHandler handlers.AuthenticationErrorHandler
	mapper       authapi.UserIdentityMapper
}

func NewExternalOAuthRedirector(provider Provider, state State, redirectURL string, success handlers.AuthenticationSuccessHandler, errorHandler handlers.AuthenticationErrorHandler, mapper authapi.UserIdentityMapper) (handlers.AuthenticationRedirector, http.Handler, error) {
	clientConfig, err := provider.NewConfig()
	if err != nil {
		return nil, nil, err
	}

	clientConfig.RedirectUrl = redirectURL

	client, err := osincli.NewClient(clientConfig)
	if err != nil {
		return nil, nil, err
	}

	transport, err := provider.GetTransport()
	if err != nil {
		return nil, nil, err
	}
	client.Transport = transport

	handler := &Handler{
		provider:     provider,
		state:        state,
		clientConfig: clientConfig,
		client:       client,
		success:      success,
		errorHandler: errorHandler,
		mapper:       mapper,
	}

	return handler, handler, nil
}

// AuthenticationRedirect implements oauth.handlers.RedirectAuthHandler
func (h *Handler) AuthenticationRedirect(w http.ResponseWriter, req *http.Request) error {
	klog.V(4).Infof("Authentication needed for %v", h.provider)

	authReq := h.client.NewAuthorizeRequest(osincli.CODE)
	h.provider.AddCustomParameters(authReq)

	state, err := h.state.Generate(w, req)
	if err != nil {
		klog.V(4).Infof("Error generating state: %v", err)
		return err
	}

	oauthURL := authReq.GetAuthorizeUrlWithParams(state)
	klog.V(4).Infof("redirect to %v", oauthURL)

	http.Redirect(w, req, oauthURL.String(), http.StatusFound)
	return nil
}

func NewOAuthPasswordAuthenticator(provider Provider, mapper authapi.UserIdentityMapper) (openshiftauthenticator.PasswordAuthenticator, error) {
	clientConfig, err := provider.NewConfig()
	if err != nil {
		return nil, err
	}

	// unused for password grants
	clientConfig.RedirectUrl = "/"

	client, err := osincli.NewClient(clientConfig)
	if err != nil {
		return nil, err
	}

	transport, err := provider.GetTransport()
	if err != nil {
		return nil, err
	}
	client.Transport = transport

	return &Handler{
		provider:     provider,
		clientConfig: clientConfig,
		client:       client,
		mapper:       mapper,
	}, nil
}

func (h *Handler) AuthenticatePassword(ctx context.Context, username, password string) (*authenticator.Response, bool, error) {
	// Exchange password for a token
	accessReq := h.client.NewAccessRequest(osincli.PASSWORD, &osincli.AuthorizeData{Username: username, Password: password})
	accessData, err := accessReq.GetToken()
	if err != nil {
		if oauthErr, ok := err.(*osincli.Error); ok && oauthErr.Id == "invalid_grant" {
			// An invalid_grant error means the username/password was rejected
			return nil, false, nil
		}
		klog.V(2).Infof("Error getting access token from an external OIDC provider (%s) using resource owner password grant: %v", accessReq.GetTokenUrl(), err)
		return nil, false, err
	}

	klog.V(5).Infof("Got access data for %s", username)

	identity, err := h.provider.GetUserIdentity(accessData)
	if err != nil {
		klog.V(4).Infof("Error getting userIdentityInfo info: %v", err)
		return nil, false, err
	}

	return identitymapper.ResponseFor(h.mapper, identity)
}

// ServeHTTP handles the callback request in response to an external oauth flow
func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {

	// Extract auth code
	authReq := h.client.NewAuthorizeRequest(osincli.CODE)
	authData, err := authReq.HandleRequest(req)
	if err != nil {
		klog.V(4).Infof("Error handling request: %v", err)
		h.handleError(err, w, req)
		return
	}

	klog.V(4).Infof("Got auth data")

	// Validate state before making any server-to-server calls
	ok, err := h.state.Check(authData.State, req)
	if err != nil {
		klog.V(4).Infof("Error verifying state: %v", err)
		h.handleError(err, w, req)
		return
	}
	if !ok {
		klog.V(4).Infof("State is invalid")
		err := errors.New("State is invalid")
		h.handleError(err, w, req)
		return
	}

	// Exchange code for a token
	accessReq := h.client.NewAccessRequest(osincli.AUTHORIZATION_CODE, authData)
	accessData, err := accessReq.GetToken()
	if err != nil {
		klog.V(2).Infof("Error getting access token from an external OIDC provider (%s): %v", accessReq.GetTokenUrl(), err)
		h.handleError(err, w, req)
		return
	}

	klog.V(5).Infof("Got access data")
	h.login(w, req, accessData, authData.State)
}

func (h *Handler) login(w http.ResponseWriter, req *http.Request, accessData *osincli.AccessData, state string) {
	identity, err := h.provider.GetUserIdentity(accessData)
	if err != nil {
		var authorizationDeniedError api.AuthorizationDeniedError
		var authorizationFailedError api.AuthorizationFailedError
		switch {
		case errors.As(err, &authorizationDeniedError):
			klog.V(4).Infof("Authorization denied: %v", authorizationDeniedError)
			audit.AddUsernameAnnotation(req, authorizationDeniedError.Identity().GetProviderPreferredUserName())
			audit.AddDecisionAnnotation(req, audit.DenyDecision)
			h.handleError(err, w, req)

		case errors.As(err, &authorizationFailedError):
			klog.V(4).Infof("Authorization failed: %v", authorizationFailedError)
			audit.AddUsernameAnnotation(req, authorizationFailedError.Identity().GetProviderPreferredUserName())
			audit.AddDecisionAnnotation(req, audit.ErrorDecision)
			h.handleError(err, w, req)

		default:
			klog.V(4).Infof("Error getting userIdentityInfo info: %v", err)
			audit.AddDecisionAnnotation(req, audit.ErrorDecision)
			h.handleError(err, w, req)
		}
		return
	}

	userInfo, err := h.mapper.UserFor(identity)
	if err != nil {
		klog.V(4).Infof("Error creating or updating mapping for: %#v due to %v", identity, err)
		audit.AddDecisionAnnotation(req, audit.ErrorDecision)
		h.handleError(err, w, req)
		return
	}
	klog.V(4).Infof("Got userIdentityMapping: %#v", userInfo)
	audit.AddUsernameAnnotation(req, userInfo.GetName())
	audit.AddDecisionAnnotation(req, audit.AllowDecision)

	_, err = h.success.AuthenticationSucceeded(userInfo, state, w, req)
	if err != nil {
		klog.V(4).Infof("Error calling success handler: %v", err)
		h.handleError(err, w, req)
		return
	}
}

func (h *Handler) handleError(err error, w http.ResponseWriter, req *http.Request) {
	handled, _ := h.errorHandler.AuthenticationError(err, w, req)
	if handled {
		return
	}

	klog.V(4).Infof("handle error failed for err: %v", err)
	http.Error(w, "An error occured", http.StatusInternalServerError)
}

// defaultState provides default state-building, validation, and parsing to contain CSRF and "then" redirection
type defaultState struct {
	csrf csrf.CSRF
}

// RedirectorState combines state generation/verification with redirections on authentication success and error
type RedirectorState interface {
	State
	handlers.AuthenticationSuccessHandler
	handlers.AuthenticationErrorHandler
}

func CSRFRedirectingState(csrf csrf.CSRF) RedirectorState {
	return &defaultState{csrf: csrf}
}

func (d *defaultState) Generate(w http.ResponseWriter, req *http.Request) (string, error) {
	then := req.URL.String()
	if len(then) == 0 {
		return "", errors.New("cannot generate state: request has no URL")
	}

	state := url.Values{
		"csrf": {d.csrf.Generate(w, req)},
		"then": {then},
	}

	return encodeState(state), nil
}

func (d *defaultState) Check(state string, req *http.Request) (bool, error) {
	values, err := decodeState(state)
	if err != nil {
		return false, err
	}

	if ok := d.csrf.Check(req, values.Get("csrf")); !ok {
		return false, fmt.Errorf("state did not contain a valid CSRF token")
	}

	if then := values.Get("then"); len(then) == 0 {
		return false, errors.New("state did not contain a redirect")
	}

	return true, nil
}

func (d *defaultState) AuthenticationSucceeded(user user.Info, state string, w http.ResponseWriter, req *http.Request) (bool, error) {
	values, err := decodeState(state)
	if err != nil {
		return false, err
	}

	then := values.Get("then")
	if len(then) == 0 {
		return false, errors.New("no redirect given")
	}

	http.Redirect(w, req, then, http.StatusFound)
	return true, nil
}

// AuthenticationError handles the very specific case where the remote OAuth provider returned an error
// In that case, attempt to redirect to the "then" URL with all error parameters echoed
// In any other case, or if an error is encountered, returns false and the original error
func (d *defaultState) AuthenticationError(err error, w http.ResponseWriter, req *http.Request) (bool, error) {
	// only handle errors that came from the remote OAuth provider...
	osinErr, ok := err.(*osincli.Error)
	if !ok {
		return false, err
	}

	// with an OAuth error...
	if len(osinErr.Id) == 0 {
		return false, err
	}

	// if they embedded valid state...
	ok, stateErr := d.Check(osinErr.State, req)
	if !ok || stateErr != nil {
		return false, err
	}

	// if the state decodes...
	values, err := decodeState(osinErr.State)
	if err != nil {
		return false, err
	}

	// if it contains a redirect...
	then := values.Get("then")
	if len(then) == 0 {
		return false, err
	}

	// which parses...
	thenURL, urlErr := url.Parse(then)
	if urlErr != nil {
		return false, err
	}

	// Add in the error, error_description, error_uri params to the "then" redirect
	q := thenURL.Query()
	q.Set("error", osinErr.Id)
	if len(osinErr.Description) > 0 {
		q.Set("error_description", osinErr.Description)
	}
	if len(osinErr.URI) > 0 {
		q.Set("error_uri", osinErr.URI)
	}
	thenURL.RawQuery = q.Encode()

	http.Redirect(w, req, thenURL.String(), http.StatusFound)

	return true, nil
}

// URL-encode, then base-64 encode for OAuth providers that don't do a good job of treating the state param like an opaque value
func encodeState(values url.Values) string {
	return base64.URLEncoding.EncodeToString([]byte(values.Encode()))
}

func decodeState(state string) (url.Values, error) {
	decodedState, err := base64.URLEncoding.DecodeString(state)
	if err != nil {
		return nil, err
	}
	return url.ParseQuery(string(decodedState))
}
