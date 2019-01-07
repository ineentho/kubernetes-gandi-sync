package main

import (
	"os"
	"sort"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tiramiseb/go-gandi-livedns"
	core_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

var options = struct {
	CloudflareAPIEmail string
	CloudflareAPIKey   string
	DNSNames           string
	NodeSelector       string
	LivednsKey         string
	HumanLogs          bool
}{
	CloudflareAPIEmail: os.Getenv("CF_API_EMAIL"),
	CloudflareAPIKey:   os.Getenv("CF_API_KEY"),
	DNSNames:           os.Getenv("DNS_NAMES"),
	NodeSelector:       os.Getenv("NODE_SELECTOR"),
	LivednsKey:         os.Getenv("GANDI_LIVEDNS_KEY"),
	HumanLogs:          os.Getenv("HUMAN_LOGS") != "",
}

func main() {
	if options.HumanLogs {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	}

	if options.LivednsKey == "" {
		log.Fatal().Msg("LIVEDNS_KEY is required")
		os.Exit(1)
	}

	dnsNames := strings.Split(options.DNSNames, ",")
	if len(dnsNames) == 1 && dnsNames[0] == "" {
		log.Fatal().Msg("DNS_NAMES is required")
		os.Exit(1)
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal().Err(err).Msg("could not create cluster config")
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("Could not create kubernetes client")
		os.Exit(1)
	}

	stop := make(chan struct{})
	defer close(stop)

	nodeSelector := labels.NewSelector()
	if options.NodeSelector != "" {
		selector, err := labels.Parse(options.NodeSelector)
		if err != nil {
			log.Fatal().Str("node_selector", options.NodeSelector).Err(err).Msg("node selector is invalid")
			os.Exit(1)
		} else {
			nodeSelector = selector
		}
	}

	factory := informers.NewSharedInformerFactory(client, time.Minute)
	lister := factory.Core().V1().Nodes().Lister()
	var lastIPs []string
	resync := func() {
		log.Debug().Msg("resyncing")
		nodes, err := lister.List(nodeSelector)
		if err != nil {
			log.Error().Err(err).Msg("failed to list nodes")
		}

		var ips []string
		for _, node := range nodes {
			if nodeIsReady(node) {
				for _, addr := range node.Status.Addresses {
					if addr.Type == core_v1.NodeExternalIP {
						ips = append(ips, addr.Address)
					}
				}
			}
		}

		sort.Strings(ips)
		if strings.Join(ips, ",") == strings.Join(lastIPs, ",") {
			log.Debug().Strs("ips", ips).Msg("no change detected")
			return
		} else {
			log.Info().Strs("ips", ips).Strs("last_ips", lastIPs).Msg("new ips detected")
		}
		lastIPs = ips

		err = sync(ips, dnsNames, options.LivednsKey)
		if err != nil {
			log.Error().Err(err).Msg("failed to sync")
		}
	}

	informer := factory.Core().V1().Nodes().Informer()
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			resync()
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			resync()
		},
		DeleteFunc: func(obj interface{}) {
			resync()
		},
	})
	informer.Run(stop)

	select {}
}

func nodeIsReady(node *core_v1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == core_v1.NodeReady && condition.Status == core_v1.ConditionTrue {
			return true
		}
	}

	return false
}

func sync(ips []string, dnsNames []string, livednsKey string) error {
	gandiClient := gandi.New(livednsKey, "")

	var records = []gandi.ZoneRecord{}

	for _, dnsName := range dnsNames {
		records = append(records, gandi.ZoneRecord{
			RrsetType:   "A",
			RrsetTTL:    300,
			RrsetName:   dnsName,
			RrsetValues: ips,
		})
	}

	_, err := gandiClient.ChangeDomainRecords("textbrawlers.com", records)

	if err != nil {
		return errors.Wrap(err, "failed to update zone records")
	}

	log.Info().Strs("dns_names", dnsNames).Strs("ips", ips).Msg("zone records updated")
	return nil
}
