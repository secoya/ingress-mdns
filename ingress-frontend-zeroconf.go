package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/fields"

	docopt "github.com/docopt/docopt-go"
	"github.com/grandcat/zeroconf"
	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

// LocalHostname An Ingress hostname in the .local domain
type LocalHostname struct {
	TLS      bool
	Hostname string
}

func main() {
	usage := `Kubernetes Ingress Frontend Zeroconf - Broadcast ingress hostnames via mDNS

Usage: broadcast [options]

Options:
  --interface=name  Interface on which to broadcast [default: eth0]
  --service=name    Ingress controller service name & namespace
                    [default: default-http-backend.kube-system]
  --cleartext-port=port  Name of port used for cleartext connections
                         to the service [default: http]
  --tls-port=port   Name of target port used for TLS connections
                    to the service [default: https]
  --kubeconfig      Use $HOME/.kube config instead of in-cluster config
  --debug           Print debugging information
  -h, --help        show this help`

	arguments, _ := docopt.ParseDoc(usage)
	debug, _ := arguments.Bool("--debug")
	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.Debug(arguments)

	interfaceName, err := arguments.String("--interface")
	if err != nil {
		log.Panic(err.Error())
	}
	broadcastInterface := getInterfaceByName(interfaceName)

	useKubeConfig, err := arguments.Bool("--kubeconfig")
	clientset := getKubernetesClientSet(useKubeConfig)

	ingressIP, portMapper := getServiceInfo(arguments, clientset)

	var zeroconfServers = map[LocalHostname]*zeroconf.Server{}
	defer unregisterAllHostnames(zeroconfServers)
	watcher := cache.NewListWatchFromClient(clientset.ExtensionsV1beta1().RESTClient(), "ingresses", v1.NamespaceAll, fields.Everything())
	log.Debugf("Watching ingresses")
	_, controller := cache.NewInformer(watcher, &v1beta1.Ingress{}, time.Second*30, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			hostnames := getIngressHostnames(obj.(*v1beta1.Ingress))
			registerHostnames(hostnames, portMapper, broadcastInterface, ingressIP, zeroconfServers)
		},
		DeleteFunc: func(obj interface{}) {
			hostnames := getIngressHostnames(obj.(*v1beta1.Ingress))
			unregisterHostnames(hostnames, zeroconfServers)
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldIngress := oldObj.(*v1beta1.Ingress)
			newIngress := oldObj.(*v1beta1.Ingress)
			oldHostnames := getIngressHostnames(oldIngress)
			newHostnames := getIngressHostnames(newIngress)
			if !reflect.DeepEqual(oldHostnames, newHostnames) {
				log.Infof("Ingress %v changed, re-registering hostnames", oldIngress.Name)
				unregisterHostnames(oldHostnames, zeroconfServers)
				registerHostnames(newHostnames, portMapper, broadcastInterface, ingressIP, zeroconfServers)
			}
		},
	})

	sigs := make(chan os.Signal, 1)
	stop := make(chan struct{})
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	go controller.Run(stop)

	go func() {
		sig := <-sigs
		log.Debugf("%v", sig)
		close(stop)
	}()
	<-stop
}

func getInterfaceByName(interfaceName string) net.Interface {
	ifaces, _ := net.Interfaces()
	ifaceNames := []string{}
	for _, iface := range ifaces {
		if iface.Name == interfaceName {
			log.Debugf("Found interface %v", interfaceName)
			return iface
		}
		ifaceNames = append(ifaceNames, iface.Name)
	}
	log.Panicf("No interface named %v was found, available interfaces are:\n%v", interfaceName, strings.Join(ifaceNames, "\n"))
	panic("")
}

func getKubernetesClientSet(useKubeConfig bool) *kubernetes.Clientset {
	var config *rest.Config
	var err error
	if useKubeConfig {
		var home string
		if home = os.Getenv("HOME"); home == "" {
			home = os.Getenv("USERPROFILE") // windows
		}
		config, err = clientcmd.BuildConfigFromFlags("", filepath.Join(home, ".kube", "config"))
		if err != nil {
			log.Panic(err.Error())
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			log.Panic(err.Error())
		}
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Panic(err.Error())
	}
	return clientset
}

func getServiceInfo(arguments docopt.Opts, clientset *kubernetes.Clientset) (net.IP, func(LocalHostname) (int, error)) {
	// Get all service info
	serviceFQName, _ := arguments.String("--service")
	if strings.Count(serviceFQName, ".") != 1 {
		log.Panicf("--service must be supplied as SERVICENAME.NAMESPACE, you gave %v", serviceFQName)
	}
	split := strings.SplitN(serviceFQName, ".", 2)
	serviceName := split[0]
	serviceNamespace := split[1]
	service, err := clientset.CoreV1().Services(serviceNamespace).Get(serviceName, metav1.GetOptions{})
	if err != nil {
		log.Panic(err.Error())
	}

	// Determine loadbalancer IP of service
	// ingressIPs := []string{}
	// for _, ingress := range service.Status.LoadBalancer.Ingress {
	// 	ingressIPs = append(ingressIPs, ingress.IP)
	// }
	// if cap(ingressIPs) != 1 {
	// 	log.Panicf("Multiple Ingress IPs for service %v to choose from (expected only one):\n%v", serviceFQName, strings.Join(ingressIPs, "\n"))
	// }
	ingressIP := net.ParseIP("10.24.79.157")
	log.Debugf("Ingress IP is %v", ingressIP)

	// Map cleartext and tls port names to numbers
	// Only map/lookup ports on demand, if one of them is not defined in
	// the service we only want to fail if an ingress actually
	// makes use of that port.
	cleartextPortName, _ := arguments.String("--cleartext-port")
	tlsPortName, _ := arguments.String("--tls-port")
	var cleartextPort int
	var tlsPort int
	for _, port := range service.Spec.Ports {
		if port.Name == cleartextPortName {
			cleartextPort = int(port.Port)
			log.Debugf("Cleartext port number is %v", cleartextPort)
		}
		if port.Name == tlsPortName {
			tlsPort = int(port.Port)
			log.Debugf("TLS port number is %v", tlsPort)
		}
	}
	return ingressIP, func(local LocalHostname) (int, error) {
		if local.TLS {
			if tlsPort != 0 {
				return tlsPort, nil
			}
			return 0, fmt.Errorf("Unable to register %v, tls target port %v "+
				"not present in ingress controller service %v",
				local.Hostname, tlsPort, serviceFQName)
		}
		if cleartextPort != 0 {
			return cleartextPort, nil
		}
		return 0, fmt.Errorf("Unable to register %v, cleartext target port %v "+
			"not present in ingress controller service %v",
			local.Hostname, cleartextPort, serviceFQName)
	}
}

func registerHostnames(
	hostnames []LocalHostname,
	portMapper func(LocalHostname) (int, error),
	broadcastInterface net.Interface,
	ingressIP net.IP,
	servers map[LocalHostname]*zeroconf.Server,
) {
	defer func() {
		if r := recover(); r != nil {
			// No need to log actual error, log.Panic should have taken care of that
			log.Errorf("Failed to register hostnames.")
		}
	}()
	for _, local := range hostnames {
		log.Infof("Registering %v", local.Hostname)
		port, err := portMapper(local)
		if err != nil {
			log.Panic(err.Error())
		}
		server, err := zeroconf.RegisterProxy(
			local.Hostname,
			"_http._tcp.",
			"local.",
			port,
			local.Hostname,
			[]string{ingressIP.String()},
			[]string{"path=/"},
			[]net.Interface{broadcastInterface},
		)
		if err != nil {
			log.Panic(err.Error())
		}
		servers[local] = server
	}
}

func unregisterHostnames(hostnames []LocalHostname, servers map[LocalHostname]*zeroconf.Server) {
	for _, local := range hostnames {
		if server, exists := servers[local]; exists {
			log.Infof("Unregistering %v", local.Hostname)
			server.Shutdown()
			delete(servers, local)
		}
	}
}

func unregisterAllHostnames(servers map[LocalHostname]*zeroconf.Server) {
	for local, server := range servers {
		log.Infof("Unregistering %v", local.Hostname)
		server.Shutdown()
	}
}

func getIngressHostnames(ingress *v1beta1.Ingress) []LocalHostname {
	// The same ingress can have both cleartext and tls hosts.
	// This is not implemented yet, for now we just check for the presence
	// of the tls.
	tls := ingress.Spec.TLS != nil
	hostnames := []LocalHostname{}
	for _, rule := range ingress.Spec.Rules {
		hostname := rule.Host
		if !strings.HasSuffix(hostname, ".kube") {
			continue
		}
		hostnames = append(hostnames, LocalHostname{tls, strings.TrimSuffix(hostname, ".kube")})
	}
	return hostnames
}
