package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	docopt "github.com/docopt/docopt-go"
	"github.com/grandcat/zeroconf"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// LocalHostname An Ingress hostname in the .local domain
type LocalHostname struct {
	TLS      bool
	Hostname string
}

func main() {
	usage := `ingress-mdns - Broadcast ingress hostnames via mDNS

Usage: ingress-mdns [options]

Options:
	--cleartext-port=port  External cleartext port
	                       of the ingress controller [default: 80]
	--tls-port=port        External TLS port
	                       of the ingress controller [default: 443]
  --debug                Print debugging information
	-h, --help             show this help

Notes:
	The service expects the environment variable $HOST_IP to be set,
	it is used to select on which interface the hostnames should be broadcast`

	arguments, _ := docopt.ParseDoc(usage)
	debug, _ := arguments.Bool("--debug")
	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		log.SetLevel(log.InfoLevel)
	}
	log.Debug(arguments)

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	broadcastIP := net.ParseIP(os.Getenv("HOST_IP"))
	broadcastInterface := getInterfaceByIP(broadcastIP)

	var zeroconfServers = map[LocalHostname]*zeroconf.Server{}
	defer unregisterAllHostnames(zeroconfServers)

	watcher := cache.NewListWatchFromClient(clientset.ExtensionsV1beta1().RESTClient(), "ingresses", v1.NamespaceAll, fields.Everything())
	log.Debugf("Watching ingresses")
	_, controller := cache.NewInformer(watcher, &v1beta1.Ingress{}, time.Second*30, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			hostnames := getIngressHostnames(obj.(*v1beta1.Ingress))
			registerHostnames(arguments, hostnames, broadcastIP, broadcastInterface, zeroconfServers)
		},
		DeleteFunc: func(obj interface{}) {
			hostnames := getIngressHostnames(obj.(*v1beta1.Ingress))
			unregisterHostnames(hostnames, zeroconfServers)
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			oldIngress := oldObj.(*v1beta1.Ingress)
			newIngress := newObj.(*v1beta1.Ingress)
			oldHostnames := getIngressHostnames(oldIngress)
			newHostnames := getIngressHostnames(newIngress)
			if !reflect.DeepEqual(oldHostnames, newHostnames) {
				log.Infof("Ingress %v changed, re-registering hostnames", oldIngress.Name)
				unregisterHostnames(oldHostnames, zeroconfServers)
				registerHostnames(arguments, newHostnames, broadcastIP, broadcastInterface, zeroconfServers)
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

func getInterfaceByIP(broadcastIP net.IP) net.Interface {
	ifaces, _ := net.Interfaces()
	ifaceIPs := []string{}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			log.Panic(err.Error())
		}
		for _, addr := range addrs {
			var ifaceIP net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ifaceIP = v.IP
			case *net.IPAddr:
				ifaceIP = v.IP
			}
			if net.IP.Equal(ifaceIP, broadcastIP) {
				log.Debugf("Found interface %v", iface.Name)
				return iface
			}
		}
		ifaceIPs = append(ifaceIPs, iface.Name)
	}
	log.Panicf("No interface with IP %v was found, available interfaces are:\n%v", broadcastIP, strings.Join(ifaceIPs, "\n"))
	panic("")
}

func registerHostnames(
	arguments docopt.Opts,
	hostnames []LocalHostname,
	broadcastIP net.IP,
	broadcastInterface net.Interface,
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
		port, _ := arguments.Int("--cleartext-port")
		if local.TLS {
			port, _ = arguments.Int("--tls-port")
		}
		server, err := zeroconf.RegisterProxy(
			local.Hostname,
			fmt.Sprintf("%s,_http._tcp", local.Hostname),
			"local.",
			port,
			local.Hostname,
			[]string{broadcastIP.String()},
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
		if !strings.HasSuffix(hostname, ".local") {
			continue
		}
		hostnames = append(hostnames, LocalHostname{tls, strings.TrimSuffix(hostname, ".local")})
	}
	return hostnames
}
