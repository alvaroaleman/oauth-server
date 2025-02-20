package gitlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/RangelReale/osincli"
	"k8s.io/klog/v2"

	authapi "github.com/openshift/oauth-server/pkg/api"
	"github.com/openshift/oauth-server/pkg/oauth/external"
)

const (
	// Uses the GitLab User-API (http://doc.gitlab.com/ce/api/users.html#current-user)
	// and OAuth-Provider (http://doc.gitlab.com/ce/integration/oauth_provider.html)
	// with default OAuth scope (http://doc.gitlab.com/ce/api/users.html#current-user)
	// Requires GitLab 7.7.0 or higher
	gitlabUserAPIPath = "/api/v3/user"
	gitlabOAuthScope  = "api"
)

type provider struct {
	providerName string
	transport    http.RoundTripper
	authorizeURL string
	tokenURL     string
	userAPIURL   string
	clientID     string
	clientSecret string
}

type gitlabUser struct {
	ID       uint64
	Username string
	Email    string
	Name     string
}

func NewOAuthProvider(providerName, URL, clientID, clientSecret string, transport http.RoundTripper) (external.Provider, error) {
	// Create service URLs
	u, err := url.Parse(URL)
	if err != nil {
		return nil, errors.New("Host URL is invalid")
	}

	return &provider{
		providerName: providerName,
		transport:    transport,
		authorizeURL: appendPath(*u, gitlabAuthorizePath),
		tokenURL:     appendPath(*u, gitlabTokenPath),
		userAPIURL:   appendPath(*u, gitlabUserAPIPath),
		clientID:     clientID,
		clientSecret: clientSecret,
	}, nil
}

func (p *provider) GetTransport() (http.RoundTripper, error) {
	return p.transport, nil
}

// NewConfig implements external/interfaces/Provider.NewConfig
func (p *provider) NewConfig() (*osincli.ClientConfig, error) {
	config := &osincli.ClientConfig{
		ClientId:                 p.clientID,
		ClientSecret:             p.clientSecret,
		ErrorsInStatusCode:       true,
		SendClientSecretInParams: true,
		AuthorizeUrl:             p.authorizeURL,
		TokenUrl:                 p.tokenURL,
		Scope:                    gitlabOAuthScope,
	}
	return config, nil
}

// AddCustomParameters implements external/interfaces/Provider.AddCustomParameters
func (p *provider) AddCustomParameters(req *osincli.AuthorizeRequest) {}

// GetUserIdentity implements external/interfaces/Provider.GetUserIdentity
func (p *provider) GetUserIdentity(data *osincli.AccessData) (authapi.UserIdentityInfo, error) {
	req, _ := http.NewRequest("GET", p.userAPIURL, nil)
	req.Header.Set("Authorization", fmt.Sprintf("bearer %s", data.AccessToken))

	client := http.DefaultClient
	if p.transport != nil {
		client = &http.Client{Transport: p.transport}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	userdata := gitlabUser{}
	err = json.Unmarshal(body, &userdata)
	if err != nil {
		return nil, err
	}

	if userdata.ID == 0 {
		return nil, errors.New("Could not retrieve GitLab id")
	}

	identity := authapi.NewDefaultUserIdentityInfo(p.providerName, fmt.Sprintf("%d", userdata.ID))
	if len(userdata.Name) > 0 {
		identity.Extra[authapi.IdentityDisplayNameKey] = userdata.Name
	}
	if len(userdata.Username) > 0 {
		identity.Extra[authapi.IdentityPreferredUsernameKey] = userdata.Username
	}
	if len(userdata.Email) > 0 {
		identity.Extra[authapi.IdentityEmailKey] = userdata.Email
	}
	klog.V(4).Infof("Got identity=%#v", identity)

	return identity, nil
}
