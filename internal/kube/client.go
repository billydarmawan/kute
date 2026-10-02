package kube

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	// Registers the OIDC auth provider, so a kubeconfig user with an
	// `auth-provider: {name: oidc}` block (Rancher, Dex, Keycloak) resolves
	// instead of failing with "no Auth Provider found for name \"oidc\"".
	// Since Kubernetes 1.26 removed the in-tree gcp/azure providers, oidc is
	// the only thing this package still registers — GKE, EKS and AKS all
	// come through the exec-plugin path (execauth.go) and need nothing here.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

// PathCheck is one kubeconfig location kute looked at and why it didn't
// yield a usable config — the 10b "LOOKED IN" box's rows.
type PathCheck struct {
	Label  string // "--kubeconfig", "$KUBECONFIG", "~/.kube/config"
	Path   string // resolved path; empty when the env var itself is unset
	Reason string // "not set", "no such file", or a parse error
}

// kubeconfigFlagPath is --kubeconfig's value, set once from main before
// anything reads a kubeconfig. A package var rather than a parameter
// threaded through every reader because it's process-wide startup input with
// exactly the same lifetime and meaning as $KUBECONFIG, and four independent
// paths resolve a kubeconfig (the client, AvailableContexts, KubeconfigPath's
// palette hint, and 10b's LOOKED IN diagnostics) — a flag one of them missed
// would silently describe a different file than the one in use.
var (
	kubeconfigFlagPathMu sync.RWMutex
	kubeconfigFlagPath   string
)

// SetKubeconfigPath pins the kubeconfig file kute reads, taking precedence
// over $KUBECONFIG (matching kubectl's own --kubeconfig precedence). Call it
// before building a client; the empty string clears the override.
func SetKubeconfigPath(path string) {
	kubeconfigFlagPathMu.Lock()
	kubeconfigFlagPath = path
	kubeconfigFlagPathMu.Unlock()
}

// kubeconfigSource resolves which kubeconfig to read and what to call it in
// user-facing diagnostics. label is "" when neither the flag nor the env var
// supplied one, leaving the caller on the ~/.kube/config default.
func kubeconfigSource() (path, label string) {
	kubeconfigFlagPathMu.RLock()
	flagPath := kubeconfigFlagPath
	kubeconfigFlagPathMu.RUnlock()
	if flagPath != "" {
		return flagPath, "--kubeconfig"
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env, "$KUBECONFIG"
	}
	return "", ""
}

// explicitKubeconfigPath is kubeconfigSource's path-only form, for the
// loading rules — the flag, else $KUBECONFIG, else "".
func explicitKubeconfigPath() string {
	path, _ := kubeconfigSource()
	return path
}

// kubeconfigLoadingRules builds the loading rules every kubeconfig read in
// this package shares: the resolved file (--kubeconfig, else $KUBECONFIG,
// else ~/.kube/config) as the explicit path, and — deliberately — no
// migration rules.
//
// clientcmd's defaults carry a legacy migration rule: "if ~/.kube/config is
// missing but the pre-1.0 location has one, copy it across". Load() runs it
// before reading anything, including when ExplicitPath points at a completely
// different file, so it fires on every read kute does. On Windows its source
// is %HOME%\.kube\config while its destination is homedir.HomeDir()'s pick of
// %HOME%/%HOMEDRIVE%%HOMEPATH%/%USERPROFILE% — routinely two different files,
// so reading any kubeconfig would copy one home's config over another's, and
// when the destination's .kube directory doesn't exist the copy fails and the
// read returns *that* error instead of the config. That is what emptied the
// 7a context palette on Windows CI ("0 contexts", straight from
// AvailableContexts erroring out) as soon as a test redirected $HOME. kute
// reads kubeconfigs; it has no business writing one as a side effect, on any
// platform.
func kubeconfigLoadingRules() *clientcmd.ClientConfigLoadingRules {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.MigrationRules = nil
	if explicit := explicitKubeconfigPath(); explicit != "" {
		rules.ExplicitPath = explicit
	} else if path := defaultKubeconfigPath(); path != "" {
		rules.ExplicitPath = path
	}
	return rules
}

// defaultKubeconfigPath is ~/.kube/config, or "" when the home directory
// can't be determined.
func defaultKubeconfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

// ConfigLookupError is returned when no kubeconfig could be found anywhere
// kute looks and in-cluster config also isn't available (10b) — a
// structured alternative to the raw clientcmd error so the setup screen can
// show exactly what was checked instead of one opaque message.
type ConfigLookupError struct {
	Paths []PathCheck
	cause error
}

func (e *ConfigLookupError) Error() string {
	return fmt.Sprintf("no kubeconfig found: %v", e.cause)
}

func (e *ConfigLookupError) Unwrap() error { return e.cause }

func buildConfigLookupError(explicitPath, explicitLabel, defaultPath string, cause error) *ConfigLookupError {
	var checks []PathCheck
	if explicitPath == "" {
		// Neither --kubeconfig nor $KUBECONFIG supplied a path. Report the
		// env var, since that's the one the user can set without re-running.
		checks = append(checks, PathCheck{Label: "$KUBECONFIG", Reason: "not set"})
	} else {
		checks = append(checks, PathCheck{Label: explicitLabel, Path: explicitPath, Reason: pathFailureReason(explicitPath)})
	}
	if defaultPath != "" {
		checks = append(checks, PathCheck{Label: "~/.kube/config", Path: defaultPath, Reason: pathFailureReason(defaultPath)})
	}
	return &ConfigLookupError{Paths: checks, cause: cause}
}

func pathFailureReason(path string) string {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "no such file"
		}
		return err.Error()
	}
	return "invalid kubeconfig"
}

type Client struct {
	Interface  kubernetes.Interface
	RESTConfig *rest.Config
	Context    Context
	// credentialConfig is retained for diagnostics and package tests only.
	// RESTConfig is the anonymous outer config whose transport already
	// contains credentials, keeping every derived client behind the gate.
	credentialConfig *rest.Config
	authGate         *authenticationGate
	watcher          *watchObserver
	// pluginStderr is this context's exec-plugin stderr capture, nil when it
	// has no exec plugin (see stderrCaptureFor).
	pluginStderr *stderrCapture
}

func NewClient() (Client, error) {
	// Empty context override means "use the kubeconfig's current-context".
	return newClientForContext("")
}

// NewClientForContext builds a Client pinned to the named kubeconfig context,
// overriding the file's current-context. It backs Cluster.SwitchContext.
func NewClientForContext(contextName string) (Client, error) {
	return newClientForContext(contextName)
}

func newClientForContext(contextName string) (Client, error) {
	loadingRules := kubeconfigLoadingRules()
	explicitPath, explicitLabel := kubeconfigSource()
	defaultPath := defaultKubeconfigPath()

	configOverrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		configOverrides.CurrentContext = contextName
	}
	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		if inClusterConfig, inClusterErr := rest.InClusterConfig(); inClusterErr == nil {
			restConfig = inClusterConfig
		} else {
			return Client{}, buildConfigLookupError(explicitPath, explicitLabel, defaultPath, err)
		}
	}

	// client-go's defaults (QPS 5, Burst 10) trip client-side throttling
	// under the app's 2s metrics polling; raise them so requests aren't
	// queued for seconds at a time (k9s uses the same order of magnitude).
	restConfig.QPS = 50
	restConfig.Burst = 100

	// GKE/EKS credentials are minted by an exec plugin that would otherwise
	// be handed this process's stdin and stderr — see execauth.go. Must
	// happen before the first client is built from this config: that build is
	// what creates (and globally caches) client-go's Authenticator, freezing
	// both decisions for the rest of the session.
	prepareExecProvider(restConfig)
	credentialConfig := restConfig
	authGate := &authenticationGate{staticCredentials: !usesCredentialPlugin(restConfig)}
	watcher := newWatchObserver()

	var clientset *kubernetes.Clientset
	pluginStderr := stderrCaptureFor(restConfig)
	pluginStderr.capture(func() {
		var authenticatedTransport http.RoundTripper
		authenticatedTransport, err = rest.TransportFor(restConfig)
		if err != nil {
			return
		}
		// Wrap outside the already-built credential transport. WrapTransport
		// itself is inside client-go's auth wrappers and would still invoke an
		// expired exec plugin before the gate got a chance to reject traffic.
		controlled := rest.AnonymousClientConfig(restConfig)
		controlled.TLSClientConfig = rest.TLSClientConfig{}
		controlled.Dial = nil
		controlled.Proxy = nil
		controlled.Transport = &observingRoundTripper{next: authenticatedTransport, gate: authGate, observer: watcher}
		restConfig = controlled
		clientset, err = kubernetes.NewForConfig(restConfig)
	})
	if err != nil {
		return Client{}, err
	}

	rawConfig, err := clientConfig.RawConfig()
	ctx := Context{ClusterName: "cluster", Namespace: "default"}
	if err == nil {
		// RawConfig() reflects the kubeconfig file's own current-context
		// verbatim — configOverrides above only ever reaches the REST config
		// clientConfig.ClientConfig() built, never this raw view — so an
		// explicit contextName override must win here too, or every field
		// derived from it below (ClusterName, Namespace, AuthInfo) silently
		// describes the file's default context instead of the one actually
		// requested (found via 4c's SwitchToContext: the header kept naming
		// the old context after a successful switch to a different one).
		currentName := rawConfig.CurrentContext
		if contextName != "" {
			currentName = contextName
		}
		ctx.ContextName = currentName
		if current, ok := rawConfig.Contexts[currentName]; ok {
			ctx.ClusterName = current.Cluster
			ctx.Namespace = current.Namespace
			if ctx.Namespace == "" {
				ctx.Namespace = "default"
			}
			ctx.UserName = current.AuthInfo
			if authInfo, ok := rawConfig.AuthInfos[current.AuthInfo]; ok {
				if cn, orgs, ok := clientCertIdentity(authInfo); ok {
					// The client cert's Subject is the identity the API
					// server actually authenticates — often nothing like
					// the kubeconfig user entry's own name (see UserName's
					// doc comment). This is a strictly better answer than
					// the AuthInfo name whenever it's available.
					ctx.UserName = cn
					ctx.UserGroups = orgs
				}
			}
		}
	}

	return Client{Interface: clientset, RESTConfig: restConfig, Context: ctx, credentialConfig: credentialConfig, authGate: authGate, watcher: watcher, pluginStderr: pluginStderr}, nil
}

// clientCertIdentity extracts the real identity a client-certificate
// AuthInfo authenticates as: the leaf certificate's Subject CommonName
// (the username) and Organization fields (Kubernetes' own convention for
// encoding a client cert's group memberships — see
// k8s.io/apiserver's x509 authenticator). ok is false for any other auth
// mode (token, exec plugin, OIDC, …) or a cert that fails to parse.
func clientCertIdentity(authInfo *clientcmdapi.AuthInfo) (commonName string, organizations []string, ok bool) {
	data := authInfo.ClientCertificateData
	if len(data) == 0 && authInfo.ClientCertificate != "" {
		var err error
		data, err = os.ReadFile(authInfo.ClientCertificate)
		if err != nil {
			return "", nil, false
		}
	}
	if len(data) == 0 {
		return "", nil, false
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", nil, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", nil, false
	}
	if cert.Subject.CommonName == "" {
		return "", nil, false
	}
	return cert.Subject.CommonName, cert.Subject.Organization, true
}

// KubeconfigPath returns the kubeconfig file path kute resolves
// (--kubeconfig, else $KUBECONFIG, else ~/.kube/config), and whether it could
// be determined — for the context palette's right-hand hint (7a:
// "~/.kube/config · 5 contexts").
func KubeconfigPath() (string, bool) {
	if explicit := explicitKubeconfigPath(); explicit != "" {
		return explicit, true
	}
	if path := defaultKubeconfigPath(); path != "" {
		return path, true
	}
	return "", false
}

// AvailableContexts returns the kubeconfig's context names (sorted) and the
// current-context name, for the context switcher. It reads the merged kubeconfig
// without contacting any cluster.
func AvailableContexts() (names []string, current string, err error) {
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(kubeconfigLoadingRules(), &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return nil, "", err
	}
	names = slices.Sorted(maps.Keys(raw.Contexts))
	return names, raw.CurrentContext, nil
}
