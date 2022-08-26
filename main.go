package main

import (
	"context"
	_ "expvar"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	_ "expvar"

	"github.com/cyverse-de/go-mod/cfg"
	"github.com/cyverse-de/go-mod/logging"
	"github.com/cyverse-de/go-mod/otelutils"
	"github.com/cyverse-de/go-mod/protobufjson"
	"github.com/cyverse-de/monitoring-agent/natsconn"
	"github.com/knadh/koanf"
	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
)

const serviceName = "monitoring-agent"

var log = logging.Log.WithFields(logrus.Fields{"service": serviceName})

func initNATS(c *koanf.Koanf, envPrefix *string) (*natsconn.Connector, error) {
	natsCluster := c.String("nats.cluster")
	if natsCluster == "" {
		log.Fatalf("The %sNATS_CLUSTER environment variable or nats.cluster configuration value must be set", *envPrefix)
	}

	tlsCert := c.String("nats.tls.cert")
	if tlsCert == "" {
		log.Fatalf("The %sNATS_TLS_CERT environment variable or nats.tlscert configuration value must be set", *envPrefix)
	}

	tlsKey := c.String("nats.tls.key")
	if tlsKey == "" {
		log.Fatalf("The %sNATS_TLS_KEY environment variable or nats.tlskey configuration value must be set", *envPrefix)
	}

	caCert := c.String("nats.tls.ca.cert")
	if caCert == "" {
		log.Fatalf("The %sNATS_TLS_CA_CERT environment variable or nats.cacert configuration value must be set", *envPrefix)
	}

	credsPath := c.String("nats.creds.path")
	if credsPath == "" {
		log.Fatalf("The %sNATS_CREDS_PATH environment variable or nats.creds configuration value must be set", *envPrefix)
	}

	maxReconnects := c.Int("nats.reconnects.max")
	reconnectWait := c.Int("nats.reconnects.wait")

	natsSubject := c.String("nats.basesubject")
	if natsSubject == "" {
		log.Fatalf("The %sNATS_BASESUBJECT environment variable or nats.basesubject configuration value must be set", *envPrefix)
	}

	natsQueue := c.String("nats.basequeue")
	if natsQueue == "" {
		log.Fatalf("The %sNATS_BASEQUEUE environment variable or nats.basequeue configuration value must be set", *envPrefix)
	}

	log.Infof("nats.cluster is set to '%s'", natsCluster)
	log.Infof("NATS TLS cert file is %s", tlsCert)
	log.Infof("NATS TLS key file is %s", tlsKey)
	log.Infof("NATS CA cert file is %s", caCert)
	log.Infof("NATS creds file is %s", credsPath)
	log.Infof("NATS max reconnects is %d", maxReconnects)
	log.Infof("NATS reonnect wait is %d", reconnectWait)

	natsConn, err := natsconn.NewConnector(&natsconn.ConnectorSettings{
		BaseSubject:   natsSubject,
		BaseQueue:     natsQueue,
		NATSCluster:   natsCluster,
		CredsPath:     credsPath,
		TLSKeyPath:    tlsKey,
		TLSCertPath:   tlsCert,
		CAPath:        caCert,
		MaxReconnects: maxReconnects,
		ReconnectWait: reconnectWait,
	})
	if err != nil {
		log.Fatal(err)
	}

	return natsConn, err
}

func initDNSChecks(c *koanf.Koanf) ([]string, time.Duration, error) {
	dnsChecksSetting := c.String("dns.hostnames")
	dnsCheckHostnames := make([]string, 0)
	if dnsChecksSetting != "" {
		dnsChecksParts := strings.Split(dnsChecksSetting, ",")
		for _, p := range dnsChecksParts {
			dnsCheckHostnames = append(dnsCheckHostnames, strings.TrimSpace(p))
		}
	}

	checkIntervalSetting := c.String("dns.checkinterval")
	if checkIntervalSetting == "" {
		checkIntervalSetting = "1m"
	}
	intervalD, err := time.ParseDuration(checkIntervalSetting)

	return dnsCheckHostnames, intervalD, err
}

func main() {
	var (
		err error
		c   *koanf.Koanf

		configPath = flag.String("config", cfg.DefaultConfigPath, "The path to the config file")
		dotEnvPath = flag.String("dotenv-path", cfg.DefaultDotEnvPath, "The path to the env file to load")
		logLevel   = flag.String("log-level", "info", "One of trace, debug, info, warn, error, fatal, or panic.")
		envPrefix  = flag.String("env-prefix", cfg.DefaultEnvPrefix, "The prefix to look for when setting configuration setting in environment variables")
		varsPort   = flag.Int("vars-port", 60000, "The port to listen on for requests to /debug/vars")
	)
	flag.Parse()

	logging.SetupLogging(*logLevel)

	tracerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdown := otelutils.TracerProviderFromEnv(tracerCtx, serviceName, func(e error) { log.Fatal(e) })
	defer shutdown()

	nats.RegisterEncoder("protojson", protobufjson.NewCodec(protobufjson.WithEmitUnpopulated()))

	c, err = cfg.Init(&cfg.Settings{
		EnvPrefix:   *envPrefix,
		ConfigPath:  *configPath,
		DotEnvPath:  *dotEnvPath,
		StrictMerge: false,
		FileType:    cfg.YAML,
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("Done reading config from %s", *configPath)

	dnsHosts, dnsInterval, err := initDNSChecks(c)
	if err != nil {
		log.Fatal(err)
	}

	natsConn, err := initNATS(c, envPrefix)
	if err != nil {
		log.Fatal(err)
	}

	pingSubject, pingQueue, err := natsConn.Subscribe("ping", func(m *nats.Msg) {
		log.Info("ping message received")
		err := m.Respond([]byte("pong"))
		if err != nil {
			log.Error(err)
		}
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("subscribed to %s on queue %s via NATS", pingSubject, pingQueue)

	// The periodic DNS health checks. We aren't using a Ticker because we only want
	// a single check to run at a time and have the interval control the time between
	// iterations.
	go func(dnsHosts []string, interval time.Duration) {
		log = log.WithFields(logrus.Fields{"context": "dns check"})
		for {
			for _, h := range dnsHosts {
				addrs, err := net.LookupHost(h)
				if err != nil {
					log.Error(err)
					continue
				}
				for _, addr := range addrs {
					log.Infof("host: %s, addr: %s", h, addr)
				}
			}

			// Add logic for sending check results here.

			time.Sleep(dnsInterval)
		}
	}(dnsHosts, dnsInterval)

	portStr := fmt.Sprintf(":%d", *varsPort)
	if err = http.ListenAndServe(portStr, nil); err != nil {
		log.Fatal(err)
	}
}
