package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/intstr"

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
  --cleartext-port=port  Target port used for cleartext connections
                         to the service [default: 80]
  --tls-port=port   Target port used for TLS connections to the service
                    [default: 443]
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
	broadcastInterface, broadcastIPs := getBroadcastInterface(interfaceName)

	useKubeConfig, err := arguments.Bool("--kubeconfig")
	clientset := getKubernetesClientSet(useKubeConfig)

	portMapper := getPortMapper(arguments, clientset)

	var zeroconfServers = map[LocalHostname]*zeroconf.Server{}
	defer unregisterAllHostnames(zeroconfServers)
	watcher := cache.NewListWatchFromClient(clientset.ExtensionsV1beta1().RESTClient(), "ingresses", v1.NamespaceAll, fields.Everything())
	_, controller := cache.NewInformer(watcher, &v1beta1.Ingress{}, time.Second*30, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			hostnames := getIngressHostnames(obj.(*v1beta1.Ingress))
			registerHostnames(hostnames, portMapper, broadcastInterface, broadcastIPs, zeroconfServers)
		},
		DeleteFunc: func(obj interface{}) {
			hostnames := getIngressHostnames(obj.(*v1beta1.Ingress))
			unregisterHostnames(hostnames, zeroconfServers)
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldHostnames := getIngressHostnames(oldObj.(*v1beta1.Ingress))
			newHostnames := getIngressHostnames(newObj.(*v1beta1.Ingress))
			if !reflect.DeepEqual(oldHostnames, newHostnames) {
				unregisterHostnames(oldHostnames, zeroconfServers)
				registerHostnames(newHostnames, portMapper, broadcastInterface, broadcastIPs, zeroconfServers)
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

func getBroadcastInterface(interfaceName string) (net.Interface, []string) {
	log.Debugf("Determining IPs of %v", interfaceName)
	ifaces, _ := net.Interfaces()
	ifaceList := ""
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		addresses := []string{}
		for _, addr := range addrs {
			split := strings.SplitN(addr.String(), "/", 2)
			ip := split[0]
			addresses = append(addresses, ip)
		}
		if iface.Name == interfaceName {
			log.Debugf("Broadcast IPs are: %v", strings.Join(addresses, ", "))
			return iface, addresses
		}
		ifaceList = ifaceList + fmt.Sprintf("%v (IPs: %v)\n", iface.Name, strings.Join(addresses, ", "))
	}
	log.Panicf("No IP found for interface %v, available interfaces are:\n%v", interfaceName, ifaceList)
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

func getPortMapper(arguments docopt.Opts, clientset *kubernetes.Clientset) func(LocalHostname) (int, error) {
	// Only map/lookup ports on demand, if one of them is not defined in
	// the service we only want to fail if an ingress actually
	// makes use of that port.
	serviceFQName, _ := arguments.String("--service")
	portMap := getPortMap(clientset, serviceFQName)
	cleartextPort, tlsPort := getPortArgs(arguments)
	return func(local LocalHostname) (int, error) {
		if local.TLS {
			if nodePort, exists := portMap[tlsPort]; exists {
				return nodePort, nil
			}
			return 0, fmt.Errorf("Unable to register %v, tls target port %v "+
				"not present in ingress controller service %v",
				local.Hostname, tlsPort, serviceFQName)
		}
		if nodePort, exists := portMap[cleartextPort]; exists {
			return nodePort, nil
		}
		return 0, fmt.Errorf("Unable to register %v, cleartext target port %v "+
			"not present in ingress controller service %v",
			local.Hostname, cleartextPort, serviceFQName)
	}
}

func getPortMap(clientset *kubernetes.Clientset, serviceFQName string) map[intstr.IntOrString]int {
	log.Debugf("Getting port mapping for %v", serviceFQName)
	if strings.Count(serviceFQName, ".") != 1 {
		log.Panicf("--service must be supplied as SERVICENAME.NAMESPACE, you gave %v", serviceFQName)
	}
	split := strings.SplitN(serviceFQName, ".", 2)
	serviceName := split[0]
	namespace := split[1]
	service, err := clientset.CoreV1().Services(namespace).Get(serviceName, metav1.GetOptions{})
	portMap := make(map[intstr.IntOrString]int)
	if err != nil {
		log.Panic(err.Error())
	}
	for _, port := range service.Spec.Ports {
		portMap[port.TargetPort] = int(port.NodePort)
	}
	log.Debugf("Port mapping for %v is:\n %v", serviceFQName, portMap)
	return portMap
}

func getPortArgs(arguments docopt.Opts) (intstr.IntOrString, intstr.IntOrString) {
	var cleartextPort intstr.IntOrString
	var tlsPort intstr.IntOrString
	cleartextPortStr, _ := arguments.String("--cleartext-port")
	if cleartextPortInt, err := strconv.Atoi(cleartextPortStr); err == nil {
		cleartextPort = intstr.FromInt(cleartextPortInt)
	} else {
		cleartextPort = intstr.FromString(cleartextPortStr)
	}
	tlsPortStr, _ := arguments.String("--tls-port")
	if tlsPortInt, err := strconv.Atoi(tlsPortStr); err == nil {
		tlsPort = intstr.FromInt(tlsPortInt)
	} else {
		tlsPort = intstr.FromString(tlsPortStr)
	}
	return cleartextPort, tlsPort
}

func registerHostnames(
	hostnames []LocalHostname,
	portMapper func(LocalHostname) (int, error),
	broadcastInterface net.Interface,
	broadcastIPs []string,
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
			broadcastIPs,
			[]string{"txtv=0", "lo=1", "la=2"},
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
		if !strings.HasSuffix(hostname, ".local") {
			continue
		}
		hostnames = append(hostnames, LocalHostname{tls, strings.TrimSuffix(hostname, ".local")})
	}
	return hostnames
}
