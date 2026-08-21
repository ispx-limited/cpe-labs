// Package mqtt implements the TR-369 MQTT Message Transfer Protocol binding
// for a simulated agent.
//
// Two details of the binding are load-bearing and easy to get wrong:
//
//   - MQTT 3.1.1, not 5.0. TR-369 4.2 prefers 5.0's response-topic property,
//     but brokers in the wild are frequently 3.1.1 only (NATS-native MQTT
//     among them), and 3.1.1 has no user properties. The fallback the spec
//     defines for 3.1.1 is the reply-to-in-topic convention below.
//   - R-MQTT.24 reply-to. A controller publishes to
//     "<agent-topic>/reply-to=<url-encoded controller topic>", so an agent
//     cannot know its inbound topics up front and MUST subscribe with a
//     wildcard. We publish the mirror image so the controller learns where to
//     answer us.
package mqtt

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// Topic layout, per the MTP mapping every controller we target uses: an agent
// subscribes to its own inbox and publishes to the controller's.
const (
	agentTopicPrefix      = "usp/v1/agent/"
	controllerTopicPrefix = "usp/v1/controller/"
)

// Config describes one agent's MQTT connection.
type Config struct {
	BrokerAddr string // host:port, no scheme
	EndpointID string // the agent's TR-369 endpoint id
	// ControllerTopic is where the agent publishes. Defaults to the agent's
	// own slot on the controller inbox, which is what a broker ACL that pins
	// an identity to its own subject expects.
	ControllerTopic string
	Username        string
	Password        string
	// HMACSecret derives the password when Password is empty, per the
	// shared-secret scheme controllers use to admit an agent that has no
	// per-device credential yet: password = base64url-nopad(HMAC-SHA256(
	// secret, endpoint-id)).
	HMACSecret   string
	KeepAlive    time.Duration
	ConnectRetry time.Duration
	Logger       *slog.Logger
}

// Client is one agent's MQTT session. Safe for concurrent use.
type Client struct {
	cfg    Config
	client paho.Client
	log    *slog.Logger

	mu       sync.Mutex
	onRecord func(payload []byte)
	// replyTo is the controller topic learned from an inbound reply-to
	// suffix, which takes precedence over the configured topic: the
	// controller is telling us where it wants the answer.
	replyTo string
}

// AgentTopic is the inbox for an endpoint id.
func AgentTopic(endpointID string) string { return agentTopicPrefix + endpointID }

// ControllerTopic is the agent's slot on the controller inbox.
func ControllerTopic(endpointID string) string { return controllerTopicPrefix + endpointID }

// DerivePassword implements the shared-secret scheme described on
// Config.HMACSecret.
func DerivePassword(secret, username string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(username))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// EnableLibraryLogging routes the MQTT library's internal ERROR and CRITICAL
// logs into the supplied logger.
//
// Off by default because the library is chatty, but worth having: paho reports
// broker-side disconnects and protocol errors through these loggers rather than
// through the connection-lost handler, so without it a session that the broker
// has closed can look healthy from the client's side.
func EnableLibraryLogging(log *slog.Logger) {
	paho.ERROR = slogWriter{log: log, level: slog.LevelError}
	paho.CRITICAL = slogWriter{log: log, level: slog.LevelError}
	paho.WARN = slogWriter{log: log, level: slog.LevelWarn}
}

type slogWriter struct {
	log   *slog.Logger
	level slog.Level
}

func (w slogWriter) Println(v ...any) {
	w.log.Log(context.Background(), w.level, "usp/mqtt lib: "+fmt.Sprint(v...))
}
func (w slogWriter) Printf(format string, v ...any) {
	w.log.Log(context.Background(), w.level, "usp/mqtt lib: "+fmt.Sprintf(format, v...))
}

// New builds a client. It does not connect; call Connect.
func New(cfg Config) (*Client, error) {
	if cfg.BrokerAddr == "" {
		return nil, fmt.Errorf("usp/mqtt: broker address is required")
	}
	if cfg.EndpointID == "" {
		return nil, fmt.Errorf("usp/mqtt: endpoint id is required")
	}
	if cfg.ControllerTopic == "" {
		cfg.ControllerTopic = ControllerTopic(cfg.EndpointID)
	}
	if cfg.Username == "" {
		cfg.Username = cfg.EndpointID
	}
	if cfg.Password == "" && cfg.HMACSecret != "" {
		cfg.Password = DerivePassword(cfg.HMACSecret, cfg.Username)
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 60 * time.Second
	}
	if cfg.ConnectRetry <= 0 {
		cfg.ConnectRetry = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Client{cfg: cfg, log: cfg.Logger}, nil
}

// OnRecord registers the handler for inbound USP record payloads. Set it before
// Connect so no message is missed between subscribing and the first delivery.
func (c *Client) OnRecord(fn func(payload []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onRecord = fn
}

// Connect dials the broker, subscribes to the agent's wildcard inbox and blocks
// until connected or ctx is done.
func (c *Client) Connect(ctx context.Context) error {
	opts := paho.NewClientOptions().
		AddBroker("tcp://" + c.cfg.BrokerAddr).
		SetClientID(c.cfg.EndpointID).
		SetUsername(c.cfg.Username).
		SetPassword(c.cfg.Password).
		SetProtocolVersion(4). // 4 == MQTT 3.1.1
		SetKeepAlive(c.cfg.KeepAlive).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(c.cfg.ConnectRetry).
		SetOnConnectHandler(c.onConnect).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			c.log.Warn("usp/mqtt: connection lost", "endpoint_id", c.cfg.EndpointID, "err", err.Error())
		})

	c.client = paho.NewClient(opts)

	token := c.client.Connect()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tokenDone(token):
		if err := token.Error(); err != nil {
			return fmt.Errorf("usp/mqtt: connect to %s: %w", c.cfg.BrokerAddr, err)
		}
	}
	return nil
}

// onConnect subscribes on every successful connect, including reconnects: a
// clean session drops server-side subscription state, so re-subscribing here
// rather than once after Connect is what keeps an agent reachable after a
// broker restart.
func (c *Client) onConnect(client paho.Client) {
	// The "/#" is required, not defensive: see R-MQTT.24 in the package doc.
	topic := AgentTopic(c.cfg.EndpointID) + "/#"
	token := client.Subscribe(topic, 0, func(_ paho.Client, m paho.Message) {
		c.handleMessage(m)
	})
	go func() {
		<-tokenDone(token)
		if err := token.Error(); err != nil {
			c.log.Error("usp/mqtt: subscribe failed",
				"endpoint_id", c.cfg.EndpointID, "topic", topic, "err", err.Error())
			return
		}
		c.log.Info("usp/mqtt: connected",
			"endpoint_id", c.cfg.EndpointID,
			"broker", c.cfg.BrokerAddr,
			"subscribed", topic,
			"publishes_to", c.cfg.ControllerTopic)
	}()
}

func (c *Client) handleMessage(m paho.Message) {
	if replyTo := parseReplyTo(m.Topic()); replyTo != "" {
		c.mu.Lock()
		c.replyTo = replyTo
		c.mu.Unlock()
	}
	c.mu.Lock()
	fn := c.onRecord
	c.mu.Unlock()
	if fn != nil {
		fn(m.Payload())
	}
}

// parseReplyTo extracts the controller topic a controller appended to our
// inbox topic. Returns "" when the topic carries no reply-to segment.
func parseReplyTo(topic string) string {
	const marker = "/reply-to="
	i := indexOf(topic, marker)
	if i < 0 {
		return ""
	}
	raw := topic[i+len(marker):]
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		return raw
	}
	return decoded
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Publish sends one USP record to the controller.
//
// The published topic carries our own reply-to suffix so the controller knows
// which topic to answer on, which is the other half of R-MQTT.24.
func (c *Client) Publish(payload []byte) error {
	if c.client == nil || !c.client.IsConnected() {
		return fmt.Errorf("usp/mqtt: not connected")
	}
	c.mu.Lock()
	base := c.replyTo
	c.mu.Unlock()
	if base == "" {
		base = c.cfg.ControllerTopic
	}
	topic := base + "/reply-to=" + url.QueryEscape(AgentTopic(c.cfg.EndpointID))

	token := c.client.Publish(topic, 1, false, payload)
	if !token.WaitTimeout(publishTimeout) {
		// The library keeps the QoS 1 message and resends it when the
		// session comes back, so the record is not lost by returning
		// here. What the caller needs is the answer now: a notify that
		// cannot be acknowledged is a failed delivery, which is the signal
		// the bulk data collector retains a report on. Waiting for the
		// acknowledgement instead blocked the caller for as long as the
		// broker was away, and a report collected while blocked was never
		// taken.
		return fmt.Errorf("usp/mqtt: publish to %s: no acknowledgement within %s", topic, publishTimeout)
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("usp/mqtt: publish to %s: %w", topic, err)
	}
	return nil
}

// publishTimeout bounds how long Publish waits for the broker's PUBACK.
// Keepalive detection of a dead session takes a keepalive interval plus
// the ping timeout, which is longer than any caller should sit on a send;
// a broker that has not acknowledged in ten seconds is not going to.
const publishTimeout = 10 * time.Second

// Disconnect closes the session, waiting briefly for in-flight publishes.
func (c *Client) Disconnect() {
	if c.client != nil && c.client.IsConnected() {
		c.client.Disconnect(250)
	}
}

// tokenDone adapts a paho token to a channel so callers can select on ctx.
func tokenDone(t paho.Token) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		t.Wait()
		close(ch)
	}()
	return ch
}
