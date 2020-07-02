package servicemirror

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynamic "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/clientcmd"

	controllerK8s "github.com/linkerd/linkerd2/controller/k8s"
	"github.com/linkerd/linkerd2/pkg/admin"
	"github.com/linkerd/linkerd2/pkg/flags"
	"github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/multicluster"
	"github.com/linkerd/linkerd2/pkg/servicemirror"
	log "github.com/sirupsen/logrus"
)

var clusterWatcher *RemoteClusterServiceWatcher

// Main executes the service-mirror controller
func Main(args []string) {
	cmd := flag.NewFlagSet("service-mirror", flag.ExitOnError)

	kubeConfigPath := cmd.String("kubeconfig", "", "path to the local kube config")
	requeueLimit := cmd.Int("event-requeue-limit", 3, "requeue limit for events")
	metricsAddr := cmd.String("metrics-addr", ":9999", "address to serve scrapable metrics on")
	namespace := cmd.String("namespace", "", "namespace containing Link and credentials Secret")
	repairPeriod := cmd.Duration("endpoint-refresh-period", 1*time.Minute, "frequency to refresh endpoint resolution")

	flags.ConfigureAndParse(cmd, args)
	linkName := cmd.Arg(0)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	k8sAPI, err := k8s.NewAPI(*kubeConfigPath, "", "", []string{}, 0)
	//TODO: Use can-i to check for required permissions
	if err != nil {
		log.Fatalf("Failed to initialize K8s API: %s", err)
	}

	controllerK8sAPI, err := controllerK8s.InitializeAPI(*kubeConfigPath, false,
		controllerK8s.NS,
		controllerK8s.Svc,
		controllerK8s.Endpoint,
	)
	if err != nil {
		log.Fatalf("Failed to initialize K8s API: %s", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "multicluster.linkerd.io",
		Version:  "v1alpha1",
		Resource: "links",
	}
	linkClient := k8sAPI.DynamicClient.Resource(gvr).Namespace(*namespace)

	go admin.StartServer(*metricsAddr)

	controllerK8sAPI.Sync(nil)

	// Start link watch
	linkWatch, err := linkClient.Watch(metav1.ListOptions{})
	if err != nil {
		log.Fatalf("Failed to watch Link %s: %s", linkName, err)
	}
	results := linkWatch.ResultChan()
	for {
		select {
		case event := <-results:
			switch obj := event.Object.(type) {
			case *dynamic.Unstructured:
				if obj.GetName() == linkName {
					switch event.Type {
					case watch.Added, watch.Modified:
						link, err := multicluster.NewLink(*obj)
						if err != nil {
							log.Errorf("Failed to parse link %s: %s", linkName, err)
							continue
						}
						log.Infof("Got updated link %s: %+v", linkName, link)
						creds, err := loadCredentials(link, *namespace, k8sAPI)
						if err != nil {
							log.Errorf("Failed to load remote cluster credentials: %s", err)
						}
						restartClusterWatcher(link, *namespace, creds, controllerK8sAPI, *requeueLimit, *repairPeriod)
					case watch.Deleted:
						log.Infof("Link %s deleted", linkName)
					default:
						log.Infof("Ignoring event type %s", event.Type)
					}
				}
			default:
				log.Errorf("Unknown object type detected: %+v", obj)
			}
		}
	}
}

func loadCredentials(link multicluster.Link, namespace string, k8sAPI *k8s.KubernetesAPI) (*servicemirror.WatchedClusterConfig, error) {
	// Load the credentials secret
	secret, err := k8sAPI.Interface.CoreV1().Secrets(namespace).Get(link.ClusterCredentialsSecret, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Failed to load credentials secret %s: %s", link.ClusterCredentialsSecret, err)
	}
	return servicemirror.ParseRemoteClusterSecret(secret)
}

func restartClusterWatcher(
	link multicluster.Link,
	namespace string,
	creds *servicemirror.WatchedClusterConfig,
	controllerK8sAPI *controllerK8s.API,
	requeueLimit int,
	repairPeriod time.Duration,
) {
	if clusterWatcher != nil {
		clusterWatcher.Stop(false)
	}

	cfg, err := clientcmd.RESTConfigFromKubeConfig(creds.APIConfig)
	if err != nil {
		log.Errorf("Unable to parse kube config: %s", err)
		return
	}

	clusterWatcher, err = NewRemoteClusterServiceWatcher(
		namespace,
		controllerK8sAPI,
		cfg,
		link.TargetClusterName,
		requeueLimit,
		repairPeriod,
		link.TargetClusterDomain,
	)
	if err != nil {
		log.Errorf("Unable to create cluster watcher: %s", err)
		return
	}

	err = clusterWatcher.Start()
	if err != nil {
		log.Errorf("Failed to start cluster watcher: %s", err)
	}
}
