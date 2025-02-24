package util

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"

	//"runtime/debug"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	g "github.com/onsi/ginkgo"
	o "github.com/onsi/gomega"
	"github.com/pborman/uuid"
	"github.com/tidwall/gjson"

	kubeauthorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage/names"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	watchtools "k8s.io/client-go/tools/watch"
	"k8s.io/client-go/util/flowcontrol"
	e2e "k8s.io/kubernetes/test/e2e/framework"

	oauthv1 "github.com/openshift/api/oauth/v1"
	projectv1 "github.com/openshift/api/project/v1"
	userv1 "github.com/openshift/api/user/v1"
	appsv1client "github.com/openshift/client-go/apps/clientset/versioned"
	authorizationv1client "github.com/openshift/client-go/authorization/clientset/versioned"
	buildv1client "github.com/openshift/client-go/build/clientset/versioned"
	configv1client "github.com/openshift/client-go/config/clientset/versioned"
	imagev1client "github.com/openshift/client-go/image/clientset/versioned"
	oauthv1client "github.com/openshift/client-go/oauth/clientset/versioned"
	operatorv1client "github.com/openshift/client-go/operator/clientset/versioned"
	projectv1client "github.com/openshift/client-go/project/clientset/versioned"
	quotav1client "github.com/openshift/client-go/quota/clientset/versioned"
	routev1client "github.com/openshift/client-go/route/clientset/versioned"
	securityv1client "github.com/openshift/client-go/security/clientset/versioned"
	templatev1client "github.com/openshift/client-go/template/clientset/versioned"
	userv1client "github.com/openshift/client-go/user/clientset/versioned"
)

// CLI provides function to call the OpenShift CLI and Kubernetes and OpenShift
// clients.
type CLI struct {
	execPath               string
	verb                   string
	configPath             string
	currentWs              WorkSpace
	guestConfigPath        string
	adminConfigPath        string
	orgServerURL           string // User orgnaization workspace server url
	homeServerURL          string // User homes workspace server url
	username               string
	globalArgs             []string
	commandArgs            []string
	finalArgs              []string
	workSpacesToDelete     []WorkSpace
	stdin                  *bytes.Buffer
	stdout                 io.Writer
	stderr                 io.Writer
	verbose                bool
	showInfo               bool
	withoutNamespace       bool
	withoutKubeconf        bool
	withoutWorkSpaceServer bool
	asGuestKubeconf        bool
	kubeFramework          *e2e.Framework

	resourcesToDelete []resourceRef
}

type resourceRef struct {
	Resource  schema.GroupVersionResource
	Namespace string
	Name      string
}

// WorkSpace defination
type WorkSpace struct {
	Name            string // WorkSpace Name            E.g. e2e-test-kcp-workspace-xxxxx
	ServerURL       string // WorkSpace ServerURL       E.g. https://{{kcp-service-domain}}/clusters/root:orgID:e2e-test-kcp-workspace-xxxxx
	ParentServerURL string // WorkSpace ParentServerURL E.g. https://{{kcp-service-domain}}/clusters/root:orgID
}

var (
	loadConfigOnce sync.Once
	testContext    string
	orgServer      string
	homeServer     string
)

// loadConfig gets the User orgnaization workspace and home workspace servers
func loadConfig() {
	var err error
	// If not set "E2E_TEST_CONTEXT" use "kcp-stable" testContext by default
	testContext = os.Getenv("E2E_TEST_CONTEXT")
	if testContext == "" {
		testContext = "kcp-stable"
		e2e.Debugf(`Env var "E2E_TEST_CONTEXT" does not exist, using kcp-stable context`)
	}
	configJSON := ReadKubeConfig(KubeConfigPath())
	orgServer = gjson.Get(configJSON, `clusters.#(name="`+testContext+`").cluster.server`).String()
	e2e.Debugf(`User orgnaization workspace server is: "%s"`, orgServer)
	client := &CLI{
		execPath:               "kubectl",
		withoutNamespace:       true,
		withoutKubeconf:        true,
		withoutWorkSpaceServer: true,
		showInfo:               false,
		adminConfigPath:        KubeConfigPath(),
	}
	rootServer := GetParentWsServerURL(orgServer)
	homeServer, err = client.Run("get").Args("workspace/~", "--server="+rootServer, "-o=jsonpath={.status.URL}").Output()
	if err != nil {
		e2e.Logf(`Getting home workspace server failed: "%v"`, err)
	}
	e2e.Debugf(`User home workspace server is: "%s"`, homeServer)
}

// GetParentWsServerURL returns the parentServer of the input server URL
func GetParentWsServerURL(serverURL string) string {
	tempSlice := strings.Split(serverURL, ":")
	return strings.Join(tempSlice[:(len(tempSlice)-1)], ":")
}

// ReadKubeConfig returns a specific kubeconfig to JSON
func ReadKubeConfig(kubeconfigPath string) string {
	output, err := ioutil.ReadFile(kubeconfigPath)
	if err != nil {
		e2e.Logf(`Reading kubeconfig file failed: "%v"`, err)
	}
	output, err = yaml.YAMLToJSON(output)
	if err != nil {
		e2e.Logf(`Parsing kubeconfig file failed: "%v"`, err)
	}
	return string(output)
}

// NewCLI initialize the upstream E2E framework and set the namespace to match
// with the project name. Note that this function does not initialize the project
// role bindings for the namespace.
func NewCLI(project, adminConfigPath string) *CLI {
	client := &CLI{}

	// must be registered before the e2e framework aftereach
	g.AfterEach(client.TeardownProject)

	client.kubeFramework = e2e.NewDefaultFramework(project)
	client.kubeFramework.SkipNamespaceCreation = true
	client.username = "admin"
	client.execPath = "kubectl"
	client.showInfo = true
	client.adminConfigPath = adminConfigPath

	g.BeforeEach(client.SetupProject)

	return client
}

// NewCLIWithWorkSpace initialize the upstream E2E framework with adding a
// workspace. You may also call SetupWorkSpace() to create a new one.
// The workspace named "e2e-test-"" + wsPrefix + 5Bytes random string
// E.g. e2e-test-kcp-workspace-bfzjr
func NewCLIWithWorkSpace(wsPrefix string) *CLI {
	client := &CLI{}

	// must be registered before the e2e framework aftereach
	g.AfterEach(client.TeardownWorkSpace)
	client.kubeFramework = e2e.NewDefaultFramework(wsPrefix)
	client.kubeFramework.SkipNamespaceCreation = true
	client.execPath = "kubectl"
	client.adminConfigPath = KubeConfigPath()
	client.showInfo = true
	// Load config once every case
	loadConfigOnce.Do(loadConfig)
	client.homeServerURL = homeServer
	client.orgServerURL = orgServer
	client.currentWs = WorkSpace{Name: "homeWorkSpace", ServerURL: client.homeServerURL, ParentServerURL: ""}
	// Create a workspace for kcp test before each case execute
	g.BeforeEach(client.SetupWorkSpace)

	return client
}

// KubeFramework returns Kubernetes framework which contains helper functions
// specific for Kubernetes resources
func (c *CLI) KubeFramework() *e2e.Framework {
	return c.kubeFramework
}

// Username returns the name of currently logged user. If there is no user assigned
// for the current session, it returns 'admin'.
func (c *CLI) Username() string {
	return c.username
}

// AsAdmin changes current config file path to the admin config.
func (c *CLI) AsAdmin() *CLI {
	nc := *c
	nc.configPath = c.adminConfigPath
	return &nc
}

// ChangeUser changes the user used by the current CLI session.
func (c *CLI) ChangeUser(name string) *CLI {
	clientConfig := c.GetClientConfigForUser(name)

	kubeConfig, err := createConfig(c.Namespace(), clientConfig)
	if err != nil {
		FatalErr(err)
	}

	f, err := ioutil.TempFile("", "configfile")
	if err != nil {
		FatalErr(err)
	}
	c.configPath = f.Name()
	err = clientcmd.WriteToFile(*kubeConfig, c.configPath)
	if err != nil {
		FatalErr(err)
	}

	c.username = name
	e2e.Logf("configPath is now %q", c.configPath)
	return c
}

// SetNamespace sets a new namespace
func (c *CLI) SetNamespace(ns string) *CLI {
	c.kubeFramework.Namespace = &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}
	return c
}

// NotShowInfo instructs the command will not be logged
func (c *CLI) NotShowInfo() *CLI {
	c.showInfo = false
	return c
}

// SetGuestKubeconf instructs the guest cluster kubeconf file is set
func (c *CLI) SetGuestKubeconf(guestKubeconf string) *CLI {
	c.guestConfigPath = guestKubeconf
	return c
}

// WithoutNamespace instructs the command should be invoked without adding --namespace parameter
func (c CLI) WithoutNamespace() *CLI {
	c.withoutNamespace = true
	return &c
}

// WithoutWorkSpaceServer instructs the command should be invoked without adding --server parameter
func (c CLI) WithoutWorkSpaceServer() *CLI {
	c.withoutWorkSpaceServer = true
	return &c
}

// WithoutKubeconf instructs the command should be invoked without adding --kubeconfig parameter
func (c CLI) WithoutKubeconf() *CLI {
	c.withoutKubeconf = true
	return &c
}

// AsGuestKubeconf instructs the command should take kubeconfig of guest cluster
func (c CLI) AsGuestKubeconf() *CLI {
	c.asGuestKubeconf = true
	c.withoutNamespace = true // if you want to use guest cluster config to opeate guest cluster, you have to set
	// withoutNamespace as true (like calling WithoutNamespace), so you can not get ns of
	// management cluster, and you have to set ns of guest cluster in Args.
	return &c
}

// SetupProject creates a new project and assign a random user to the project.
// All resources will be then created within this project.
func (c *CLI) SetupProject() {
	newNamespace := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", c.kubeFramework.BaseName))
	c.SetNamespace(newNamespace).ChangeUser(fmt.Sprintf("%s-user", newNamespace))
	e2e.Logf("The user is now %q", c.Username())

	e2e.Logf("Creating project %q", newNamespace)
	_, err := c.ProjectClient().ProjectV1().ProjectRequests().Create(&projectv1.ProjectRequest{
		ObjectMeta: metav1.ObjectMeta{Name: newNamespace},
	})
	o.Expect(err).NotTo(o.HaveOccurred())

	c.kubeFramework.AddNamespacesToDelete(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: newNamespace}})

	e2e.Logf("Waiting on permissions in project %q ...", newNamespace)
	err = WaitForSelfSAR(1*time.Second, 60*time.Second, c.KubeClient(), kubeauthorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &kubeauthorizationv1.ResourceAttributes{
			Namespace: newNamespace,
			Verb:      "create",
			Group:     "",
			Resource:  "pods",
		},
	})
	o.Expect(err).NotTo(o.HaveOccurred())

	// Wait for SAs and default dockercfg Secret to be injected
	// TODO: it would be nice to have a shared list but it is defined in at least 3 place,
	// TODO: some of them not even using the constants
	DefaultServiceAccounts := []string{
		"default",
		"deployer",
		"builder",
	}
	for _, sa := range DefaultServiceAccounts {
		e2e.Logf("Waiting for ServiceAccount %q to be provisioned...", sa)
		err = WaitForServiceAccount(c.KubeClient().CoreV1().ServiceAccounts(newNamespace), sa)
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	var ctx context.Context
	cancel := func() {}
	defer func() { cancel() }()
	// Wait for default role bindings for those SAs
	for _, name := range []string{"system:image-pullers", "system:image-builders", "system:deployers"} {
		e2e.Logf("Waiting for RoleBinding %q to be provisioned...", name)

		ctx, cancel = watchtools.ContextWithOptionalTimeout(context.Background(), 3*time.Minute)

		fieldSelector := fields.OneTermEqualSelector("metadata.name", name).String()
		lw := &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = fieldSelector
				return c.KubeClient().RbacV1().RoleBindings(newNamespace).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = fieldSelector
				return c.KubeClient().RbacV1().RoleBindings(newNamespace).Watch(options)
			},
		}

		_, err := watchtools.UntilWithSync(ctx, lw, &rbacv1.RoleBinding{}, nil, func(event watch.Event) (b bool, e error) {
			switch t := event.Type; t {
			case watch.Added, watch.Modified:
				return true, nil

			case watch.Deleted:
				return true, fmt.Errorf("object has been deleted")

			default:
				return true, fmt.Errorf("internal error: unexpected event %#v", e)
			}
		})
		o.Expect(err).NotTo(o.HaveOccurred())
	}

	e2e.Logf("Project %q has been fully provisioned.", newNamespace)
}

// CreateProject creates a new project and assign a random user to the project.
// All resources will be then created within this project.
// TODO this should be removed.  It's only used by image tests.
func (c *CLI) CreateProject() string {
	newNamespace := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", c.kubeFramework.BaseName))
	e2e.Logf("Creating project %q", newNamespace)
	_, err := c.ProjectClient().ProjectV1().ProjectRequests().Create(&projectv1.ProjectRequest{
		ObjectMeta: metav1.ObjectMeta{Name: newNamespace},
	})
	o.Expect(err).NotTo(o.HaveOccurred())

	actualNs, err := c.AdminKubeClient().CoreV1().Namespaces().Get(newNamespace, metav1.GetOptions{})
	o.Expect(err).NotTo(o.HaveOccurred())
	c.kubeFramework.AddNamespacesToDelete(actualNs)

	e2e.Logf("Waiting on permissions in project %q ...", newNamespace)
	err = WaitForSelfSAR(1*time.Second, 60*time.Second, c.KubeClient(), kubeauthorizationv1.SelfSubjectAccessReviewSpec{
		ResourceAttributes: &kubeauthorizationv1.ResourceAttributes{
			Namespace: newNamespace,
			Verb:      "create",
			Group:     "",
			Resource:  "pods",
		},
	})
	o.Expect(err).NotTo(o.HaveOccurred())
	return newNamespace
}

// TeardownProject removes projects created by this test.
func (c *CLI) TeardownProject() {
	e2e.TestContext.DumpLogsOnFailure = os.Getenv("DUMP_EVENTS_ON_FAILURE") != "false"
	if len(c.Namespace()) > 0 && g.CurrentGinkgoTestDescription().Failed && e2e.TestContext.DumpLogsOnFailure {
		e2e.DumpAllNamespaceInfo(c.kubeFramework.ClientSet, c.Namespace())
	}

	if len(c.configPath) > 0 {
		os.Remove(c.configPath)
	}

	dynamicClient := c.AdminDynamicClient()
	for _, resource := range c.resourcesToDelete {
		err := dynamicClient.Resource(resource.Resource).Namespace(resource.Namespace).Delete(resource.Name, nil)
		e2e.Logf("Deleted %v, err: %v", resource, err)
	}
}

// SetupWorkSpace creates a new WorkSpace under the org workspace
func (c *CLI) SetupWorkSpace() {
	c.SetupWorkSpaceWithSpecificPath(c.homeServerURL)
}

// SetupWorkSpaceWithSpecificPath creates a new WorkSpace with specific paths
func (c *CLI) SetupWorkSpaceWithSpecificPath(serverURL string) {
	newWorkSpace := names.SimpleNameGenerator.GenerateName(fmt.Sprintf("e2e-test-%s-", c.kubeFramework.BaseName))
	e2e.Logf("Creating workspace %q", newWorkSpace)
	output, errinfo := c.WithoutNamespace().WithoutKubeconf().WithoutWorkSpaceServer().Run("ws").Args("create", "--server="+serverURL, newWorkSpace).Output()
	o.Expect(errinfo).NotTo(o.HaveOccurred())
	o.Expect(output).Should(o.ContainSubstring("is ready to use"))
	c.currentWs.Name = newWorkSpace
	c.currentWs.ParentServerURL = serverURL
	c.currentWs.ServerURL = serverURL + ":" + newWorkSpace
	// Add the workspace to teardown deleted list
	c.workSpacesToDelete = append(c.workSpacesToDelete, c.currentWs)
	e2e.Logf("Workspace %q has been fully provisioned.", c.currentWs.Name)
}

// TeardownWorkSpace removes workspaces created by this test.
func (c *CLI) TeardownWorkSpace() {
	if len(c.configPath) > 0 {
		os.Remove(c.configPath)
	}
	if !(os.Getenv("DELETE_WORKSPACE") == "false") {
		// Sort the need to delete workSpaces delete the deepest level firstly
		sort.Slice(c.workSpacesToDelete, func(i, j int) bool {
			return len(c.workSpacesToDelete[i].ParentServerURL) > len(c.workSpacesToDelete[j].ParentServerURL)
		})
		e2e.Debugf("***%v***", c.workSpacesToDelete)
		for _, ws := range c.workSpacesToDelete {
			err := c.WithoutNamespace().WithoutKubeconf().WithoutWorkSpaceServer().Run("delete").Args("--server="+ws.ParentServerURL, "workspace", ws.Name).Execute()
			e2e.Logf("Deleted %v, err: %v", ws.Name, err)
		}
	}
}

// Verbose turns on printing verbose messages when executing OpenShift commands
func (c *CLI) Verbose() *CLI {
	c.verbose = true
	return c
}

// RESTMapper method
func (c *CLI) RESTMapper() meta.RESTMapper {
	ret := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(c.KubeClient().Discovery()))
	ret.Reset()
	return ret
}

// AppsClient method
func (c *CLI) AppsClient() appsv1client.Interface {
	return appsv1client.NewForConfigOrDie(c.UserConfig())
}

// AuthorizationClient method
func (c *CLI) AuthorizationClient() authorizationv1client.Interface {
	return authorizationv1client.NewForConfigOrDie(c.UserConfig())
}

// BuildClient method
func (c *CLI) BuildClient() buildv1client.Interface {
	return buildv1client.NewForConfigOrDie(c.UserConfig())
}

// ImageClient method
func (c *CLI) ImageClient() imagev1client.Interface {
	return imagev1client.NewForConfigOrDie(c.UserConfig())
}

// ProjectClient method
func (c *CLI) ProjectClient() projectv1client.Interface {
	return projectv1client.NewForConfigOrDie(c.UserConfig())
}

// QuotaClient method
func (c *CLI) QuotaClient() quotav1client.Interface {
	return quotav1client.NewForConfigOrDie(c.UserConfig())
}

// RouteClient method
func (c *CLI) RouteClient() routev1client.Interface {
	return routev1client.NewForConfigOrDie(c.UserConfig())
}

// TemplateClient method
func (c *CLI) TemplateClient() templatev1client.Interface {
	return templatev1client.NewForConfigOrDie(c.UserConfig())
}

// AdminAppsClient method
func (c *CLI) AdminAppsClient() appsv1client.Interface {
	return appsv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminAuthorizationClient method
func (c *CLI) AdminAuthorizationClient() authorizationv1client.Interface {
	return authorizationv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminBuildClient method
func (c *CLI) AdminBuildClient() buildv1client.Interface {
	return buildv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminConfigClient method
func (c *CLI) AdminConfigClient() configv1client.Interface {
	return configv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminImageClient method
func (c *CLI) AdminImageClient() imagev1client.Interface {
	return imagev1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminOauthClient method
func (c *CLI) AdminOauthClient() oauthv1client.Interface {
	return oauthv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminOperatorClient method
func (c *CLI) AdminOperatorClient() operatorv1client.Interface {
	return operatorv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminProjectClient method
func (c *CLI) AdminProjectClient() projectv1client.Interface {
	return projectv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminQuotaClient method
func (c *CLI) AdminQuotaClient() quotav1client.Interface {
	return quotav1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminOAuthClient method
func (c *CLI) AdminOAuthClient() oauthv1client.Interface {
	return oauthv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminRouteClient method
func (c *CLI) AdminRouteClient() routev1client.Interface {
	return routev1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminUserClient method
func (c *CLI) AdminUserClient() userv1client.Interface {
	return userv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminSecurityClient method
func (c *CLI) AdminSecurityClient() securityv1client.Interface {
	return securityv1client.NewForConfigOrDie(c.AdminConfig())
}

// AdminTemplateClient method
func (c *CLI) AdminTemplateClient() templatev1client.Interface {
	return templatev1client.NewForConfigOrDie(c.AdminConfig())
}

// KubeClient provides a Kubernetes client for the current namespace
func (c *CLI) KubeClient() kubernetes.Interface {
	return kubernetes.NewForConfigOrDie(c.UserConfig())
}

// DynamicClient method
func (c *CLI) DynamicClient() dynamic.Interface {
	return dynamic.NewForConfigOrDie(c.UserConfig())
}

// AdminKubeClient provides a Kubernetes client for the cluster admin user.
func (c *CLI) AdminKubeClient() kubernetes.Interface {
	return kubernetes.NewForConfigOrDie(c.AdminConfig())
}

// AdminDynamicClient method
func (c *CLI) AdminDynamicClient() dynamic.Interface {
	return dynamic.NewForConfigOrDie(c.AdminConfig())
}

// UserConfig method
func (c *CLI) UserConfig() *rest.Config {
	clientConfig, err := getClientConfig(c.configPath)
	if err != nil {
		FatalErr(err)
	}
	return clientConfig
}

// AdminConfig method
func (c *CLI) AdminConfig() *rest.Config {
	clientConfig, err := getClientConfig(c.adminConfigPath)
	if err != nil {
		FatalErr(err)
	}
	return clientConfig
}

// Namespace returns the name of the namespace used in the current test case.
// If the namespace is not set, an empty string is returned.
func (c *CLI) Namespace() string {
	if c.kubeFramework.Namespace == nil {
		return ""
	}
	return c.kubeFramework.Namespace.Name
}

// WorkSpace returns the workspace used in the current test case.
func (c *CLI) WorkSpace() WorkSpace {
	return c.currentWs
}

// OrgServerURL returns the user orgnaization workspace server url.
func (c *CLI) OrgServerURL() string {
	return c.orgServerURL
}

// HomeServerURL returns the user home workspace server url.
func (c *CLI) HomeServerURL() string {
	return c.homeServerURL
}

// setOutput allows to override the default command output
func (c *CLI) setOutput(out io.Writer) *CLI {
	c.stdout = out
	return c
}

// Run executes given OpenShift CLI command verb (iow. "oc <verb>").
// This function also override the default 'stdout' to redirect all output
// to a buffer and prepare the global flags such as namespace and config path.
func (c *CLI) Run(commands ...string) *CLI {
	in, out, errout := &bytes.Buffer{}, &bytes.Buffer{}, &bytes.Buffer{}
	nc := &CLI{
		execPath:        c.execPath,
		verb:            commands[0],
		kubeFramework:   c.KubeFramework(),
		adminConfigPath: c.adminConfigPath,
		configPath:      c.configPath,
		guestConfigPath: c.guestConfigPath,
		username:        c.username,
		globalArgs:      commands,
	}
	if !c.withoutKubeconf {
		if c.asGuestKubeconf {
			if c.guestConfigPath != "" {
				nc.globalArgs = append([]string{fmt.Sprintf("--kubeconfig=%s", c.guestConfigPath)}, nc.globalArgs...)
			} else {
				FatalErr("want to use guest cluster kubeconfig, but it is not set, so please use oc.SetGuestKubeconf to set it firstly")
			}
		} else {
			nc.globalArgs = append([]string{fmt.Sprintf("--kubeconfig=%s", c.configPath)}, nc.globalArgs...)
		}
	}
	if c.asGuestKubeconf && !c.withoutNamespace {
		FatalErr("you are doing something in ns of guest cluster, please use WithoutNamespace and set ns in Args, for example, oc.AsGuestKubeconf().WithoutNamespace().Run(\"get\").Args(\"pods\", \"-n\", \"guestclusterns\").Output()")
	}
	if !c.withoutNamespace {
		nc.globalArgs = append([]string{fmt.Sprintf("--namespace=%s", c.Namespace())}, nc.globalArgs...)
	}
	// TODO: Temp solution  for parallelly ececute our test cases
	// When https://github.com/kcp-dev/kcp/issues/1689 finished
	// We could make it simply and just use the --kubeconfig instead of --server.
	if !c.withoutWorkSpaceServer {
		nc.globalArgs = append(nc.globalArgs, "--server="+c.currentWs.ServerURL)
	}
	nc.stdin, nc.stdout, nc.stderr = in, out, errout
	return nc.setOutput(c.stdout)
}

// Template sets a Go template for the OpenShift CLI command.
// This is equivalent of running "oc get foo -o template --template='{{ .spec }}'"
func (c *CLI) Template(t string) *CLI {
	if c.verb != "get" {
		FatalErr("Cannot use Template() for non-get verbs.")
	}
	templateArgs := []string{"--output=template", fmt.Sprintf("--template=%s", t)}
	commandArgs := append(c.commandArgs, templateArgs...)
	c.finalArgs = append(c.globalArgs, commandArgs...)
	return c
}

// InputString adds expected input to the command
func (c *CLI) InputString(input string) *CLI {
	c.stdin.WriteString(input)
	return c
}

// Args sets the additional arguments for the OpenShift CLI command
func (c *CLI) Args(args ...string) *CLI {
	c.commandArgs = args
	c.finalArgs = append(c.globalArgs, c.commandArgs...)
	return c
}

func (c *CLI) printCmd() string {
	return strings.Join(c.finalArgs, " ")
}

// ExitError struct
type ExitError struct {
	Cmd    string
	StdErr string
	*exec.ExitError
}

// IsDebug use for check whether the E2E_TEST "DEBUG" log enabled
func IsDebug() bool {
	logLevel := os.Getenv("E2E_TEST_LOG_LEVEL")
	return logLevel == "DEBUG"
}

// Output executes the command and returns stdout/stderr combined into one string
func (c *CLI) Output() (string, error) {
	if c.verbose {
		fmt.Printf("DEBUG: oc %s\n", c.printCmd())
	}
	cmd := exec.Command(c.execPath, c.finalArgs...)
	cmd.Stdin = c.stdin
	if c.showInfo || IsDebug() {
		e2e.Logf("Running '%s %s'", c.execPath, strings.Join(c.finalArgs, " "))
	}
	out, err := cmd.CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	e2e.Debugf("****** Output is: ******\n\"%s\"\n****** ErrorInfo is: ******\n\"%v\"", trimmed, err)
	switch err.(type) {
	case nil:
		c.stdout = bytes.NewBuffer(out)
		return trimmed, nil
	case *exec.ExitError:
		e2e.Logf("Error running %v:\n%s", cmd, trimmed)
		return trimmed, &ExitError{ExitError: err.(*exec.ExitError), Cmd: c.execPath + " " + strings.Join(c.finalArgs, " "), StdErr: trimmed}
	default:
		FatalErr(fmt.Errorf("unable to execute %q: %v", c.execPath, err))
		// unreachable code
		return "", nil
	}
}

// Outputs executes the command and returns the stdout/stderr output as separate strings
func (c *CLI) Outputs() (string, string, error) {
	if c.verbose {
		fmt.Printf("DEBUG: oc %s\n", c.printCmd())
	}
	cmd := exec.Command(c.execPath, c.finalArgs...)
	cmd.Stdin = c.stdin
	e2e.Logf("Running '%s %s'", c.execPath, strings.Join(c.finalArgs, " "))
	//out, err := cmd.CombinedOutput()
	var stdErrBuff, stdOutBuff bytes.Buffer
	cmd.Stdout = &stdOutBuff
	cmd.Stderr = &stdErrBuff
	err := cmd.Run()

	stdOutBytes := stdOutBuff.Bytes()
	stdErrBytes := stdErrBuff.Bytes()
	stdOut := strings.TrimSpace(string(stdOutBytes))
	stdErr := strings.TrimSpace(string(stdErrBytes))
	switch err.(type) {
	case nil:
		c.stdout = bytes.NewBuffer(stdOutBytes)
		c.stderr = bytes.NewBuffer(stdErrBytes)
		return stdOut, stdErr, nil
	case *exec.ExitError:
		e2e.Logf("Error running %v:\nStdOut>\n%s\nStdErr>\n%s\n", cmd, stdOut, stdErr)
		return stdOut, stdErr, err
	default:
		FatalErr(fmt.Errorf("unable to execute %q: %v", c.execPath, err))
		// unreachable code
		return "", "", nil
	}
}

// Background executes the command in the background and returns the Cmd object
// which may be killed later via cmd.Process.Kill().  It also returns buffers
// holding the stdout & stderr of the command, which may be read from only after
// calling cmd.Wait().
func (c *CLI) Background() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer, error) {
	if c.verbose {
		fmt.Printf("DEBUG: oc %s\n", c.printCmd())
	}
	cmd := exec.Command(c.execPath, c.finalArgs...)
	cmd.Stdin = c.stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = bufio.NewWriter(&stdout)
	cmd.Stderr = bufio.NewWriter(&stderr)

	e2e.Logf("Running '%s %s'", c.execPath, strings.Join(c.finalArgs, " "))

	err := cmd.Start()
	return cmd, &stdout, &stderr, err
}

// BackgroundRC executes the command in the background and returns the Cmd
// object which may be killed later via cmd.Process.Kill().  It returns a
// ReadCloser for stdout.  If in doubt, use Background().  Consult the os/exec
// documentation.
func (c *CLI) BackgroundRC() (*exec.Cmd, io.ReadCloser, error) {
	if c.verbose {
		fmt.Printf("DEBUG: oc %s\n", c.printCmd())
	}
	cmd := exec.Command(c.execPath, c.finalArgs...)
	cmd.Stdin = c.stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	e2e.Logf("Running '%s %s'", c.execPath, strings.Join(c.finalArgs, " "))

	err = cmd.Start()
	return cmd, stdout, err
}

// OutputToFile executes the command and store output to a file
func (c *CLI) OutputToFile(filename string) (string, error) {
	content, err := c.Output()
	if err != nil {
		return "", err
	}
	path := filepath.Join(e2e.TestContext.OutputDir, c.Namespace()+"-"+filename)
	return path, ioutil.WriteFile(path, []byte(content), 0644)
}

// OutputsToFiles executes the command and store the stdout in one file and stderr in another one
// The stdout output will be written to fileName+'.stdout'
// The stderr output will be written to fileName+'.stderr'
func (c *CLI) OutputsToFiles(fileName string) (string, string, error) {
	stdoutFilename := fileName + ".stdout"
	stderrFilename := fileName + ".stderr"

	stdout, stderr, err := c.Outputs()
	if err != nil {
		return "", "", err
	}
	stdoutPath := filepath.Join(e2e.TestContext.OutputDir, c.Namespace()+"-"+stdoutFilename)
	stderrPath := filepath.Join(e2e.TestContext.OutputDir, c.Namespace()+"-"+stderrFilename)

	if err := ioutil.WriteFile(stdoutPath, []byte(stdout), 0644); err != nil {
		return "", "", err
	}

	if err := ioutil.WriteFile(stderrPath, []byte(stderr), 0644); err != nil {
		return stdoutPath, "", err
	}

	return stdoutPath, stderrPath, nil
}

// Execute executes the current command and return error if the execution failed
// This function will set the default output to Ginkgo writer.
func (c *CLI) Execute() error {
	out, err := c.Output()
	if _, err := io.Copy(g.GinkgoWriter, strings.NewReader(out+"\n")); err != nil {
		fmt.Fprintln(os.Stderr, "ERROR: Unable to copy the output to ginkgo writer")
	}
	os.Stdout.Sync()
	return err
}

// FatalErr exits the test in case a fatal error has occurred.
func FatalErr(msg interface{}) {
	// the path that leads to this being called isn't always clear...
	//fmt.Fprintln(g.GinkgoWriter, string(debug.Stack()))
	//e2e.Failf("%v", msg)
}

// AddExplicitResourceToDelete method
func (c *CLI) AddExplicitResourceToDelete(resource schema.GroupVersionResource, namespace, name string) {
	c.resourcesToDelete = append(c.resourcesToDelete, resourceRef{Resource: resource, Namespace: namespace, Name: name})
}

// AddResourceToDelete method
func (c *CLI) AddResourceToDelete(resource schema.GroupVersionResource, metadata metav1.Object) {
	c.resourcesToDelete = append(c.resourcesToDelete, resourceRef{Resource: resource, Namespace: metadata.GetNamespace(), Name: metadata.GetName()})
}

// CreateUser method
func (c *CLI) CreateUser(prefix string) *userv1.User {
	user, err := c.AdminUserClient().UserV1().Users().Create(&userv1.User{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix + c.Namespace()},
	})
	if err != nil {
		FatalErr(err)
	}
	c.AddResourceToDelete(userv1.GroupVersion.WithResource("users"), user)

	return user
}

// GetClientConfigForUser method
func (c *CLI) GetClientConfigForUser(username string) *rest.Config {
	userClient := c.AdminUserClient()

	user, err := userClient.UserV1().Users().Get(username, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		FatalErr(err)
	}
	if err != nil {
		user, err = userClient.UserV1().Users().Create(&userv1.User{
			ObjectMeta: metav1.ObjectMeta{Name: username},
		})
		if err != nil {
			FatalErr(err)
		}
		c.AddResourceToDelete(userv1.GroupVersion.WithResource("users"), user)
	}

	oauthClient := c.AdminOauthClient()
	oauthClientName := "e2e-client-" + c.Namespace()
	oauthClientObj, err := oauthClient.OauthV1().OAuthClients().Create(&oauthv1.OAuthClient{
		ObjectMeta:  metav1.ObjectMeta{Name: oauthClientName},
		GrantMethod: oauthv1.GrantHandlerAuto,
	})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		FatalErr(err)
	}
	if oauthClientObj != nil {
		c.AddExplicitResourceToDelete(oauthv1.GroupVersion.WithResource("oauthclients"), "", oauthClientName)
	}

	privToken, pubToken := GenerateOAuthTokenPair()
	token, err := oauthClient.OauthV1().OAuthAccessTokens().Create(&oauthv1.OAuthAccessToken{
		ObjectMeta:  metav1.ObjectMeta{Name: pubToken},
		ClientName:  oauthClientName,
		UserName:    username,
		UserUID:     string(user.UID),
		Scopes:      []string{"user:full"},
		RedirectURI: "https://localhost:8443/oauth/token/implicit",
	})

	if err != nil {
		FatalErr(err)
	}
	c.AddResourceToDelete(oauthv1.GroupVersion.WithResource("oauthaccesstokens"), token)

	userClientConfig := rest.AnonymousClientConfig(turnOffRateLimiting(rest.CopyConfig(c.AdminConfig())))
	userClientConfig.BearerToken = privToken

	return userClientConfig
}

// GenerateOAuthTokenPair returns two tokens to use with OpenShift OAuth-based authentication.
// The first token is a private token meant to be used as a Bearer token to send
// queries to the API, the second token is a hashed token meant to be stored in
// the database.
func GenerateOAuthTokenPair() (privToken, pubToken string) {
	const sha256Prefix = "sha256~"
	randomToken := base64.RawURLEncoding.EncodeToString(uuid.NewRandom())
	hashed := sha256.Sum256([]byte(randomToken))
	return sha256Prefix + string(randomToken), sha256Prefix + base64.RawURLEncoding.EncodeToString(hashed[:])
}

// turnOffRateLimiting reduces the chance that a flaky test can be written while using this package
func turnOffRateLimiting(config *rest.Config) *rest.Config {
	configCopy := *config
	configCopy.QPS = 10000
	configCopy.Burst = 10000
	configCopy.RateLimiter = flowcontrol.NewFakeAlwaysRateLimiter()
	// We do not set a timeout because that will cause watches to fail
	// Integration tests are already limited to 5 minutes
	// configCopy.Timeout = time.Minute
	return &configCopy
}

// WaitForAccessAllowed method
func (c *CLI) WaitForAccessAllowed(review *kubeauthorizationv1.SelfSubjectAccessReview, user string) error {
	if user == "system:anonymous" {
		return waitForAccess(kubernetes.NewForConfigOrDie(rest.AnonymousClientConfig(c.AdminConfig())), true, review)
	}

	kubeClient, err := kubernetes.NewForConfig(c.GetClientConfigForUser(user))
	if err != nil {
		FatalErr(err)
	}
	return waitForAccess(kubeClient, true, review)
}

// WaitForAccessDenied method
func (c *CLI) WaitForAccessDenied(review *kubeauthorizationv1.SelfSubjectAccessReview, user string) error {
	if user == "system:anonymous" {
		return waitForAccess(kubernetes.NewForConfigOrDie(rest.AnonymousClientConfig(c.AdminConfig())), false, review)
	}

	kubeClient, err := kubernetes.NewForConfig(c.GetClientConfigForUser(user))
	if err != nil {
		FatalErr(err)
	}
	return waitForAccess(kubeClient, false, review)
}

func waitForAccess(c kubernetes.Interface, allowed bool, review *kubeauthorizationv1.SelfSubjectAccessReview) error {
	return wait.Poll(time.Second, time.Minute, func() (bool, error) {
		response, err := c.AuthorizationV1().SelfSubjectAccessReviews().Create(review)
		if err != nil {
			return false, err
		}
		return response.Status.Allowed == allowed, nil
	})
}

func getClientConfig(kubeConfigFile string) (*rest.Config, error) {
	kubeConfigBytes, err := ioutil.ReadFile(kubeConfigFile)
	if err != nil {
		return nil, err
	}
	kubeConfig, err := clientcmd.NewClientConfigFromBytes(kubeConfigBytes)
	if err != nil {
		return nil, err
	}
	clientConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, err
	}
	clientConfig.WrapTransport = defaultClientTransport

	return clientConfig, nil
}

// defaultClientTransport sets defaults for a client Transport that are suitable
// for use by infrastructure components.
func defaultClientTransport(rt http.RoundTripper) http.RoundTripper {
	transport, ok := rt.(*http.Transport)
	if !ok {
		return rt
	}

	// TODO: this should be configured by the caller, not in this method.
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport.Dial = dialer.Dial
	// Hold open more internal idle connections
	// TODO: this should be configured by the caller, not in this method.
	transport.MaxIdleConnsPerHost = 100
	return transport
}
