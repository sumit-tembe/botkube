// Copyright (c) 2019 InfraCloud Technologies
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"

	"github.com/infracloudio/botkube/pkg/config"
)

// SlackBot listens for user's message, execute commands and sends back the response
type SlackBot struct {
	log             logrus.FieldLogger
	executorFactory ExecutorFactory

	Token            string
	AllowKubectl     bool
	RestrictAccess   bool
	ClusterName      string
	ChannelName      string
	SlackURL         string
	BotID            string
	DefaultNamespace string
}

// slackMessage contains message details to execute command and send back the result
type slackMessage struct {
	log             logrus.FieldLogger
	executorFactory ExecutorFactory

	Event         *slack.MessageEvent
	BotID         string
	Request       string
	Response      string
	IsAuthChannel bool
	RTM           *slack.RTM
	SlackClient   *slack.Client
}

// NewSlackBot returns new Bot object
func NewSlackBot(log logrus.FieldLogger, c *config.Config, executorFactory ExecutorFactory) *SlackBot {
	return &SlackBot{
		log:              log,
		executorFactory:  executorFactory,
		Token:            c.Communications.Slack.Token,
		AllowKubectl:     c.Settings.Kubectl.Enabled,
		RestrictAccess:   c.Settings.Kubectl.RestrictAccess,
		ClusterName:      c.Settings.ClusterName,
		ChannelName:      c.Communications.Slack.Channel,
		DefaultNamespace: c.Settings.Kubectl.DefaultNamespace,
	}
}

// Start starts the slacknot RTM connection and listens for messages
func (b *SlackBot) Start(ctx context.Context) error {
	b.log.Info("Starting bot")
	var botID string
	api := slack.New(b.Token)
	if len(b.SlackURL) != 0 {
		api = slack.New(b.Token, slack.OptionAPIURL(b.SlackURL))
		botID = b.BotID
	} else {
		authResp, err := api.AuthTest()
		if err != nil {
			return fmt.Errorf("while testing the ability to do auth request: %w", err)
		}
		botID = authResp.UserID
	}

	rtm := api.NewRTM()
	go rtm.ManageConnection()

	for {
		select {
		case <-ctx.Done():
			b.log.Info("Shutdown requested. Finishing...")
			return rtm.Disconnect()
		case msg, ok := <-rtm.IncomingEvents:
			if !ok {
				b.log.Info("Incoming events channel closed. Finishing...")
				return nil
			}

			switch ev := msg.Data.(type) {
			case *slack.ConnectedEvent:
				b.log.Info("BotKube connected to Slack!")

			case *slack.MessageEvent:
				// Skip if message posted by BotKube
				if ev.User == botID {
					continue
				}
				sm := slackMessage{
					log:             b.log,
					executorFactory: b.executorFactory,
					Event:           ev,
					BotID:           botID,
					RTM:             rtm,
					SlackClient:     api,
				}
				err := sm.HandleMessage(b)
				if err != nil {
					wrappedErr := fmt.Errorf("while handling message: %w", err)
					b.log.Errorf(wrappedErr.Error())
				}

			case *slack.RTMError:
				b.log.Errorf("Slack RMT error: %+v", ev.Error())

			case *slack.ConnectionErrorEvent:
				b.log.Errorf("Slack connection error: %+v", ev.Error())

			case *slack.IncomingEventError:
				b.log.Errorf("Slack incoming event error: %+v", ev.Error())

			case *slack.OutgoingErrorEvent:
				b.log.Errorf("Slack outgoing event error: %+v", ev.Error())

			case *slack.UnmarshallingErrorEvent:
				b.log.Errorf("Slack unmarshalling error: %+v", ev.Error())

			case *slack.RateLimitedError:
				b.log.Errorf("Slack rate limiting error: %+v", ev.Error())

			case *slack.InvalidAuthEvent:
				return fmt.Errorf("invalid credentials")
			}
		}
	}
}

// TODO: refactor - handle and send methods should be defined on Bot level

func (sm *slackMessage) HandleMessage(b *SlackBot) error {
	// Check if message posted in authenticated channel
	info, err := sm.SlackClient.GetConversationInfo(sm.Event.Channel, true)
	if err == nil {
		if info.IsChannel || info.IsPrivate {
			// Message posted in a channel
			// Serve only if starts with mention
			if !strings.HasPrefix(sm.Event.Text, "<@"+sm.BotID+">") {
				sm.log.Debugf("Ignoring message as it doesn't contain %q prefix", sm.BotID)
				return nil
			}
			// Serve only if current channel is in config
			if b.ChannelName == info.Name {
				sm.IsAuthChannel = true
			}
		}
	}
	// Serve only if current channel is in config
	if b.ChannelName == sm.Event.Channel {
		sm.IsAuthChannel = true
	}

	// Trim the @BotKube prefix
	sm.Request = strings.TrimPrefix(sm.Event.Text, "<@"+sm.BotID+">")

	e := sm.executorFactory.NewDefault(config.SlackBot, sm.IsAuthChannel, sm.Request)
	sm.Response = e.Execute()
	err = sm.Send()
	if err != nil {
		return fmt.Errorf("while sending message: %w", err)
	}

	return nil
}

func (sm *slackMessage) Send() error {
	sm.log.Debugf("Slack incoming Request: %s", sm.Request)
	sm.log.Debugf("Slack Response: %s", sm.Response)
	if len(sm.Response) == 0 {
		return fmt.Errorf("while reading Slack response: empty response for request %q", sm.Request)
	}
	// Upload message as a file if too long
	if len(sm.Response) >= 3990 {
		params := slack.FileUploadParameters{
			Filename: sm.Request,
			Title:    sm.Request,
			Content:  sm.Response,
			Channels: []string{sm.Event.Channel},
		}
		_, err := sm.RTM.UploadFile(params)
		if err != nil {
			return fmt.Errorf("while uploading file: %w", err)
		}
		return nil
	}

	var options = []slack.MsgOption{slack.MsgOptionText(formatCodeBlock(sm.Response), false), slack.MsgOptionAsUser(true)}

	//if the message is from thread then add an option to return the response to the thread
	if sm.Event.ThreadTimestamp != "" {
		options = append(options, slack.MsgOptionTS(sm.Event.ThreadTimestamp))
	}

	if _, _, err := sm.RTM.PostMessage(sm.Event.Channel, options...); err != nil {
		return fmt.Errorf("while posting Slack message: %w", err)
	}

	return nil
}
