package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/felixge/httpsnoop"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"

	"github.com/coreos/dex/connector"
	"github.com/coreos/dex/connector/authproxy"
	"github.com/coreos/dex/connector/cf"
	"github.com/coreos/dex/connector/github"
	"github.com/coreos/dex/connector/gitlab"
	"github.com/coreos/dex/connector/ldap"
	"github.com/coreos/dex/connector/linkedin"
	"github.com/coreos/dex/connector/microsoft"
	"github.com/coreos/dex/connector/mock"
	"github.com/coreos/dex/connector/oauth"
	"github.com/coreos/dex/connector/oidc"
	"github.com/coreos/dex/connector/saml"
	"github.com/coreos/dex/storage"
)

// LocalConnector is the local passwordDB connector which is an internal
// connector maintained by the server.
const LocalConnector = "local"

// Connector is a connector with resource version metadata.
type Connector struct {
	ResourceVersion string
	Connector       connector.Connector
}

// Config holds the server's configuration options.
//
// Multiple servers using the same storage are expected to be configured identically.
type Config struct {
	Issuer string

	// The backing persistence layer.
	Storage storage.Storage

	// Valid values are "code" to enable the code flow and "token" to enable the implicit
	// flow. If no response types are supplied this value defaults to "code".
	SupportedResponseTypes []string

	// List of allowed origins for CORS requests on discovery, token and keys endpoint.
	// If none are indicated, CORS requests are disabled. Passing in "*" will allow any
	// domain.
	AllowedOrigins []string

	// If enabled, the server won't prompt the user to approve authorization requests.
	// Logging in implies approval.
	SkipApprovalScreen bool

	// If set, the server will use this connector to handle password grants
	PasswordConnector string

	RotateKeysAfter  time.Duration // Defaults to 6 hours.
	IDTokensValidFor time.Duration // Defaults to 24 hours

	GCFrequency time.Duration // Defaults to 5 minutes

	// If specified, the server will use this function for determining time.
	Now func() time.Time

	Web WebConfig

	Logger logrus.FieldLogger

	PrometheusRegistry *prometheus.Registry
}

// WebConfig holds the server's frontend templates and asset configuration.
//
// These are currently very custom to CoreOS and it's not recommended that
// outside users attempt to customize these.
type WebConfig struct {
	// A filepath to web static.
	//
	// It is expected to contain the following directories:
	//
	//   * static - Static static served at "( issuer URL )/static".
	//   * templates - HTML templates controlled by dex.
	//   * themes/(theme) - Static static served at "( issuer URL )/theme".
	//
	Dir http.FileSystem

	// Defaults to "( issuer URL )/theme/logo.png"
	LogoURL string

	// Defaults to "dex"
	Issuer string

	// Defaults to "coreos"
	Theme string
}

func value(val, defaultValue time.Duration) time.Duration {
	if val == 0 {
		return defaultValue
	}
	return val
}

// Server is the top level object.
type Server struct {
	issuerURL url.URL

	// mutex for the connectors map.
	mu sync.Mutex
	// Map of connector IDs to connectors.
	connectors map[string]Connector

	storage storage.Storage

	mux http.Handler

	templates *templates

	// If enabled, don't prompt user for approval after logging in through connector.
	skipApproval bool

	// Used for password grant
	passwordConnector string

	supportedResponseTypes map[string]bool

	now func() time.Time

	idTokensValidFor time.Duration

	logger logrus.FieldLogger
}

// NewServer constructs a server from the provided config.
func NewServer(ctx context.Context, c Config) (*Server, error) {
	return newServer(ctx, c, defaultRotationStrategy(
		value(c.RotateKeysAfter, 6*time.Hour),
		value(c.IDTokensValidFor, 24*time.Hour),
	))
}

func newServer(ctx context.Context, c Config, rotationStrategy rotationStrategy) (*Server, error) {
	if c.Logger == nil {
		c.Logger = logrus.New()
	}

	issuerURL, err := url.Parse(c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("server: can't parse issuer URL")
	}

	if c.Storage == nil {
		return nil, errors.New("server: storage cannot be nil")
	}
	if len(c.SupportedResponseTypes) == 0 {
		c.SupportedResponseTypes = []string{responseTypeCode}
	}

	supported := make(map[string]bool)
	for _, respType := range c.SupportedResponseTypes {
		switch respType {
		case responseTypeCode, responseTypeIDToken, responseTypeToken:
		default:
			return nil, fmt.Errorf("unsupported response_type %q", respType)
		}
		supported[respType] = true
	}

	if c.Now == nil {
		c.Now = time.Now
	}

	templates, err := loadTemplates(c.Web, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("server: failed to templates: %v", err)
	}

	s := &Server{
		issuerURL:              *issuerURL,
		connectors:             make(map[string]Connector),
		storage:                newKeyCacher(c.Storage, c.Now),
		supportedResponseTypes: supported,
		idTokensValidFor:       value(c.IDTokensValidFor, 24*time.Hour),
		skipApproval:           c.SkipApprovalScreen,
		now:                    c.Now,
		templates:              templates,
		passwordConnector:      c.PasswordConnector,
		logger:                 c.Logger,
	}

	// Retrieves connector objects in backend storage. This list includes the static connectors
	// defined in the ConfigMap and dynamic connectors retrieved from the storage.
	storageConnectors, err := c.Storage.ListConnectors()
	if err != nil {
		return nil, fmt.Errorf("server: failed to list connector objects from storage: %v", err)
	}

	if len(storageConnectors) == 0 && len(s.connectors) == 0 {
		return nil, errors.New("server: no connectors specified")
	}

	for _, conn := range storageConnectors {
		if _, err := s.OpenConnector(conn); err != nil {
			return nil, fmt.Errorf("server: Failed to open connector %s: %v", conn.ID, err)
		}
	}

	r := mux.NewRouter()

	handleFunc := func(p string, h http.HandlerFunc) {
		// r.HandleFunc(path.Join(issuerURL.Path, p), instrumentHandlerCounter(p, h))
		r.HandleFunc(path.Join(issuerURL.Path, p), h)
	}

	if c.PrometheusRegistry != nil {
		requestCounter := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Count of all HTTP requests.",
		}, []string{"handler", "code", "method"})

		err = c.PrometheusRegistry.Register(requestCounter)
		if err != nil {
			return nil, fmt.Errorf("server: Failed to register Prometheus HTTP metrics: %v", err)
		}
		instrumentHandlerCounter := func(handlerName string, handler http.Handler) http.HandlerFunc {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				m := httpsnoop.CaptureMetrics(handler, w, r)
				requestCounter.With(prometheus.Labels{"handler": handlerName, "code": strconv.Itoa(m.Code), "method": r.Method}).Inc()
			})
		}
		handleFunc = func(p string, h http.HandlerFunc) {
			r.HandleFunc(path.Join(issuerURL.Path, p), instrumentHandlerCounter(p, h))
		}
	}

	handlePrefix := func(p string, h http.Handler) {
		prefix := path.Join(issuerURL.Path, p)
		r.PathPrefix(prefix).Handler(http.StripPrefix(prefix, h))
	}
	handleWithCORS := func(p string, h http.HandlerFunc) {
		var handler http.Handler = h
		if len(c.AllowedOrigins) > 0 {
			corsOption := handlers.AllowedOrigins(c.AllowedOrigins)
			handler = handlers.CORS(corsOption)(handler)
		}
		r.Handle(path.Join(issuerURL.Path, p), handler)
	}
	r.NotFoundHandler = http.HandlerFunc(http.NotFound)

	discoveryHandler, err := s.discoveryHandler()
	if err != nil {
		return nil, err
	}
	handleWithCORS("/.well-known/openid-configuration", discoveryHandler)

	// TODO(ericchiang): rate limit certain paths based on IP.
	handleWithCORS("/token", s.handleToken)
	handleWithCORS("/keys", s.handlePublicKeys)
	handleFunc("/auth", s.handleAuthorization)
	handleFunc("/auth/{connector}", s.handleConnectorLogin)
	r.HandleFunc(path.Join(issuerURL.Path, "/callback"), func(w http.ResponseWriter, r *http.Request) {
		// Strip the X-Remote-* headers to prevent security issues on
		// misconfigured authproxy connector setups.
		for key := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-remote-") {
				r.Header.Del(key)
			}
		}
		s.handleConnectorCallback(w, r)
	})
	// For easier connector-specific web server configuration, e.g. for the
	// "authproxy" connector.
	handleFunc("/callback/{connector}", s.handleConnectorCallback)
	handleFunc("/approval", s.handleApproval)
	handleFunc("/healthz", s.handleHealth)
	handlePrefix("/", http.FileServer(c.Web.Dir))
	s.mux = r

	s.startKeyRotation(ctx, rotationStrategy, c.Now)
	s.startGarbageCollection(ctx, value(c.GCFrequency, 5*time.Minute), c.Now)

	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) absPath(pathItems ...string) string {
	paths := make([]string, len(pathItems)+1)
	paths[0] = s.issuerURL.Path
	copy(paths[1:], pathItems)
	return path.Join(paths...)
}

func (s *Server) absURL(pathItems ...string) string {
	u := s.issuerURL
	u.Path = s.absPath(pathItems...)
	return u.String()
}

func newPasswordDB(s storage.Storage) interface {
	connector.Connector
	connector.PasswordConnector
} {
	return passwordDB{s}
}

type passwordDB struct {
	s storage.Storage
}

func (db passwordDB) Login(ctx context.Context, s connector.Scopes, email, password string) (connector.Identity, bool, error) {
	p, err := db.s.GetPassword(email)
	if err != nil {
		if err != storage.ErrNotFound {
			return connector.Identity{}, false, fmt.Errorf("get password: %v", err)
		}
		return connector.Identity{}, false, nil
	}
	// This check prevents dex users from logging in using static passwords
	// configured with hash costs that are too high or low.
	if err := checkCost(p.Hash); err != nil {
		return connector.Identity{}, false, err
	}
	if err := bcrypt.CompareHashAndPassword(p.Hash, []byte(password)); err != nil {
		return connector.Identity{}, false, nil
	}
	return connector.Identity{
		UserID:        p.UserID,
		Username:      p.Username,
		Email:         p.Email,
		EmailVerified: true,
	}, true, nil
}

func (db passwordDB) Refresh(ctx context.Context, s connector.Scopes, identity connector.Identity) (connector.Identity, error) {
	// If the user has been deleted, the refresh token will be rejected.
	p, err := db.s.GetPassword(identity.Email)
	if err != nil {
		if err == storage.ErrNotFound {
			return connector.Identity{}, errors.New("user not found")
		}
		return connector.Identity{}, fmt.Errorf("get password: %v", err)
	}

	// User removed but a new user with the same email exists.
	if p.UserID != identity.UserID {
		return connector.Identity{}, errors.New("user not found")
	}

	// If a user has updated their username, that will be reflected in the
	// refreshed token.
	//
	// No other fields are expected to be refreshable as email is effectively used
	// as an ID and this implementation doesn't deal with groups.
	identity.Username = p.Username

	return identity, nil
}

func (db passwordDB) Prompt() string {
	return "Email Address"
}

// newKeyCacher returns a storage which caches keys so long as the next
func newKeyCacher(s storage.Storage, now func() time.Time) storage.Storage {
	if now == nil {
		now = time.Now
	}
	return &keyCacher{Storage: s, now: now}
}

type keyCacher struct {
	storage.Storage

	now  func() time.Time
	keys atomic.Value // Always holds nil or type *storage.Keys.
}

func (k *keyCacher) GetKeys() (storage.Keys, error) {
	keys, ok := k.keys.Load().(*storage.Keys)
	if ok && keys != nil && k.now().Before(keys.NextRotation) {
		return *keys, nil
	}

	storageKeys, err := k.Storage.GetKeys()
	if err != nil {
		return storageKeys, err
	}

	if k.now().Before(storageKeys.NextRotation) {
		k.keys.Store(&storageKeys)
	}
	return storageKeys, nil
}

func (s *Server) startGarbageCollection(ctx context.Context, frequency time.Duration, now func() time.Time) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(frequency):
				if r, err := s.storage.GarbageCollect(now()); err != nil {
					s.logger.Errorf("garbage collection failed: %v", err)
				} else if r.AuthRequests > 0 || r.AuthCodes > 0 {
					s.logger.Infof("garbage collection run, delete auth requests=%d, auth codes=%d", r.AuthRequests, r.AuthCodes)
				}
			}
		}
	}()
	return
}

// ConnectorConfig is a configuration that can open a connector.
type ConnectorConfig interface {
	Open(id string, logger logrus.FieldLogger) (connector.Connector, error)
}

// ConnectorsConfig variable provides an easy way to return a config struct
// depending on the connector type.
var ConnectorsConfig = map[string]func() ConnectorConfig{
	"mockCallback": func() ConnectorConfig { return new(mock.CallbackConfig) },
	"mockPassword": func() ConnectorConfig { return new(mock.PasswordConfig) },
	"ldap":         func() ConnectorConfig { return new(ldap.Config) },
	"github":       func() ConnectorConfig { return new(github.Config) },
	"gitlab":       func() ConnectorConfig { return new(gitlab.Config) },
	"oidc":         func() ConnectorConfig { return new(oidc.Config) },
	"oauth":        func() ConnectorConfig { return new(oauth.Config) },
	"saml":         func() ConnectorConfig { return new(saml.Config) },
	"cf":           func() ConnectorConfig { return new(cf.Config) },
	"authproxy":    func() ConnectorConfig { return new(authproxy.Config) },
	"linkedin":     func() ConnectorConfig { return new(linkedin.Config) },
	"microsoft":    func() ConnectorConfig { return new(microsoft.Config) },
	// Keep around for backwards compatibility.
	"samlExperimental": func() ConnectorConfig { return new(saml.Config) },
}

// openConnector will parse the connector config and open the connector.
func openConnector(logger logrus.FieldLogger, conn storage.Connector) (connector.Connector, error) {
	var c connector.Connector

	f, ok := ConnectorsConfig[conn.Type]
	if !ok {
		return c, fmt.Errorf("unknown connector type %q", conn.Type)
	}

	connConfig := f()
	if len(conn.Config) != 0 {
		data := []byte(string(conn.Config))
		if err := json.Unmarshal(data, connConfig); err != nil {
			return c, fmt.Errorf("parse connector config: %v", err)
		}
	}

	c, err := connConfig.Open(conn.ID, logger)
	if err != nil {
		return c, fmt.Errorf("failed to create connector %s: %v", conn.ID, err)
	}

	return c, nil
}

// OpenConnector updates server connector map with specified connector object.
func (s *Server) OpenConnector(conn storage.Connector) (Connector, error) {
	var c connector.Connector

	if conn.Type == LocalConnector {
		c = newPasswordDB(s.storage)
	} else {
		var err error
		c, err = openConnector(s.logger.WithField("connector", conn.Name), conn)
		if err != nil {
			return Connector{}, fmt.Errorf("failed to open connector: %v", err)
		}
	}

	connector := Connector{
		ResourceVersion: conn.ResourceVersion,
		Connector:       c,
	}
	s.mu.Lock()
	s.connectors[conn.ID] = connector
	s.mu.Unlock()

	return connector, nil
}

// getConnector retrieves the connector object with the given id from the storage
// and updates the connector list for server if necessary.
func (s *Server) getConnector(id string) (Connector, error) {
	storageConnector, err := s.storage.GetConnector(id)
	if err != nil {
		return Connector{}, fmt.Errorf("failed to get connector object from storage: %v", err)
	}

	var conn Connector
	var ok bool
	s.mu.Lock()
	conn, ok = s.connectors[id]
	s.mu.Unlock()

	if !ok || storageConnector.ResourceVersion != conn.ResourceVersion {
		// Connector object does not exist in server connectors map or
		// has been updated in the storage. Need to get latest.
		conn, err := s.OpenConnector(storageConnector)
		if err != nil {
			return Connector{}, fmt.Errorf("failed to open connector: %v", err)
		}
		return conn, nil
	}

	return conn, nil
}
