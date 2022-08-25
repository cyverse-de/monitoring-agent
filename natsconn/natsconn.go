package natsconn

import (
	"fmt"
	"strings"
	"time"

	"github.com/cyverse-de/go-mod/logging"
	"github.com/cyverse-de/go-mod/protobufjson"
	"github.com/nats-io/nats.go"
	"github.com/sirupsen/logrus"
)

var log = logging.Log.WithFields(logrus.Fields{"package": "natsconn"})

type Connector struct {
	baseSubject string
	baseQueue   string
	Conn        *nats.EncodedConn
}

type ConnectorSettings struct {
	BaseSubject   string
	BaseQueue     string
	NATSCluster   string
	CredsPath     string
	CAPath        string
	TLSKeyPath    string
	TLSCertPath   string
	MaxReconnects int
	ReconnectWait int
	EnvPrefix     string
}

func (nc *Connector) buildSubject(base string, fields ...string) string {
	trimmed := strings.TrimSuffix(
		strings.TrimSuffix(base, ".*"),
		".>",
	)
	addFields := strings.Join(fields, ".")
	return fmt.Sprintf("%s.%s", trimmed, addFields)
}

func (nc *Connector) buildQueueName(qBase string, fields ...string) string {
	return fmt.Sprintf("%s.%s", qBase, strings.Join(fields, "."))
}

func (nc *Connector) Subscribe(name string, handler nats.Handler) (string, string, error) {
	var err error

	subject := nc.buildSubject(nc.baseSubject, name)
	queue := nc.buildQueueName(nc.baseQueue, name)

	if _, err = nc.Conn.QueueSubscribe(subject, queue, handler); err != nil {
		return "", "", err
	}

	return subject, queue, nil
}

func NewConnector(cs *ConnectorSettings) (*Connector, error) {
	nats.RegisterEncoder("protojson", protobufjson.NewCodec(protobufjson.WithEmitUnpopulated()))

	nc, err := nats.Connect(
		cs.NATSCluster,
		nats.UserCredentials(cs.CredsPath),
		nats.RootCAs(cs.CAPath),
		nats.ClientCert(cs.TLSCertPath, cs.TLSKeyPath),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(cs.MaxReconnects),
		nats.ReconnectWait(time.Duration(cs.ReconnectWait)*time.Second),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			log.Errorf("disconnected from nats: %s", err.Error())
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Infof("reconnected to %s", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			log.Errorf("connection closed: %s", nc.LastError().Error())
		}),
	)
	if err != nil {
		return nil, err
	}

	ec, err := nats.NewEncodedConn(nc, "protojson")
	if err != nil {
		return nil, err
	}

	connector := &Connector{
		baseSubject: cs.BaseSubject,
		baseQueue:   cs.BaseQueue,
		Conn:        ec,
	}

	return connector, nil
}
