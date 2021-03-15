package slack

import (
	"bytes"
	"encoding/json"
	"time"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	texttemplate "text/template"

	"github.com/argoproj-labs/argocd-notifications/bot"
	
	"github.com/gorilla/websocket"

	slackclient "github.com/slack-go/slack"
)

func NewSlackAdapter(verifier RequestVerifier) *slack {
	return &slack{verifier: verifier}
}

type slack struct {
	verifier RequestVerifier
}

type ConnectedEvent struct {
	ConnectionCount int // 1 = first time, 2 = second time
	Info            *slack.SocketModeConnection
}

type DebugInfo struct {
	// Host is the name of the host name on the Slack end, that can be something like `applink-7fc4fdbb64-4x5xq`
	Host string `json:"host"`

	// `hello` type only
	BuildNumber               int `json:"build_number"`
	ApproximateConnectionTime int `json:"approximate_connection_time"`
}

type ConnectionInfo struct {
	AppID string `json:"app_id"`
}

type SocketModeMessagePayload struct {
	Event json.RawMessage `json:"´event"`
}

func mustTemplate(text string) *texttemplate.Template {
	return texttemplate.Must(texttemplate.New("usage").Parse(text))
}

// Client is a Socket Mode client that allows programs to use [Events API](https://api.slack.com/events-api)
// and [interactive components](https://api.slack.com/interactivity) over WebSocket.
// Please see [Intro to Socket Mode](https://api.slack.com/apis/connections/socket) for more information
// on Socket Mode.
//
// The implementation is highly inspired by https://www.npmjs.com/package/@slack/socket-mode,
// but the structure and the design has been adapted as much as possible to that of our RTM client for consistency
// within the library.
//
// You can instantiate the socket mode client with
// Client's New() and call Run() to start it. Please see examples/socketmode for the usage.
type Client struct {
	// Client is the main API, embedded
	apiClient slack.Client

	// maxPingInterval is the maximum duration elapsed after the last WebSocket PING sent from Slack
	// until Client considers the WebSocket connection is dead and needs to be reopened.
	maxPingInterval time.Duration

	// Connection life-cycle
	Events              chan Event
	socketModeResponses chan *Response

	// dialer is a gorilla/websocket Dialer. If nil, use the default
	// Dialer.
	dialer *websocket.Dialer

	debug bool
	log   ilogger
}

var commandsHelp = map[string]*texttemplate.Template{
	"list-subscriptions": mustTemplate("*List your subscriptions*:\n" + "```{{.cmd}} list-subscriptions```"),
	"subscribe": mustTemplate("*Subscribe current channel*:\n" +
		"```{{.cmd}} subscribe <my-app> <optional-trigger>\n" +
		"{{.cmd}} subscribe proj:<my-proj> <optional-trigger>```"),
	"unsubscribe": mustTemplate("*Unsubscribe current channel*:\n" +
		"```{{.cmd}} unsubscribe <my-app> <optional-trigger>\n" +
		"{{.cmd}} unsubscribe proj:<my-proj> <optional-trigger>```"),
}

func usageInstructions(query url.Values, command string, err error) string {
	botCommand := "/argocd"
	if cmd := query.Get("command"); cmd != "" {
		botCommand = cmd
	}

	var usage bytes.Buffer
	if err != nil {
		usage.WriteString(err.Error() + "\n")
	}

	if tmpl, ok := commandsHelp[command]; ok {
		if err := tmpl.Execute(&usage, map[string]string{"cmd": botCommand}); err != nil {
			return err.Error()
		}
	} else {
		usage.WriteString(fmt.Sprintf(":wave: Need some help with `%s`?\n", botCommand))
		for _, tmpl := range commandsHelp {
			if err := tmpl.Execute(&usage, map[string]string{"cmd": botCommand}); err != nil {
				return err.Error()
			}
			usage.WriteString("\n")
		}
	}
	return usage.String()
}

func (s *slack) parseQuery(r *http.Request) (string, url.Values, error) {
	data, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return "", nil, err
	}
	service, err := s.verifier(data, r.Header)
	if err != nil {
		return "", nil, fmt.Errorf("failed to verify request signature: %v", err)
	}
	values, err := url.ParseQuery(string(data))
	if err != nil {
		return "", nil, err
	}
	return service, values, nil
}

func (s *slack) Parse(r *http.Request) (bot.Command, error) {
	cmd := bot.Command{}
	service, query, err := s.parseQuery(r)
	if err != nil {
		return cmd, err
	}
	cmd.Service = service
	channel := query.Get("channel_name")
	if channel == "" {
		return cmd, errors.New("request does not have channel")
	}
	parts := strings.Fields(query.Get("text"))
	if len(parts) < 1 {
		return cmd, errors.New(usageInstructions(query, "", nil))
	}
	command := parts[0]

	cmd.Recipient = channel

	switch command {
	case "list-subscriptions":
		cmd.ListSubscriptions = &bot.ListSubscriptions{}
	case "subscribe", "unsubscribe":
		if len(parts) < 2 {
			return cmd, errors.New(usageInstructions(query, command, errors.New("at least one argument expected")))
		}
		update := &bot.UpdateSubscription{}
		nameParts := strings.Split(parts[1], ":")
		if len(nameParts) == 1 {
			nameParts = append([]string{"app"}, nameParts...)
		}
		switch nameParts[0] {
		case "app":
			update.App = nameParts[1]
		case "proj":
			update.Project = nameParts[1]
		default:
			return cmd, errors.New(usageInstructions(query, command, fmt.Errorf("incorrect name argument: %s", parts[1])))
		}
		if len(parts) > 2 {
			update.Trigger = parts[2]
		}
		if command == "subscribe" {
			cmd.Subscribe = update
		} else {
			cmd.Unsubscribe = update
		}
	default:
		return cmd, errors.New(usageInstructions(query, "", nil))
	}
	return cmd, nil
}

func (s *slack) SendResponse(content string, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	blocks := []slackclient.SectionBlock{{
		Type: slackclient.MBTSection,
		Text: &slackclient.TextBlockObject{Type: "mrkdwn", Text: content},
	}}
	data, err := json.Marshal(map[string]interface{}{"blocks": blocks})
	if err != nil {
		_, _ = w.Write([]byte(err.Error()))
	} else {
		_, _ = w.Write(data)
	}
}
