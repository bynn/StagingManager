package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"golang.org/x/exp/slices"
)

type stagingState int

const (
	free stagingState = iota
	taken
)

const (
	take       = "take"
	release    = "release"
	getOnQueue = "getOnQueue"
	queueNext  = "queueNext"
	override   = "override"
)

const (
	buttonDefault     = "default"
	buttonPrimary     = "primary"
	buttonDanger      = "danger"
	buttonPlainText   = "plain_text"
	buttonPrimaryText = "primary_text"
	buttonDangerText  = "danger_text"
)

type slackInteraction struct {
	channel       string
	ts            string
	currentHolder string
	queue         []string
	state         stagingState
	client        slack.Client
	timer         *time.Timer
}

func main() {
	token := "retract"
	appToken := "retract"
	client := slack.New(token, slack.OptionDebug(true), slack.OptionAppLevelToken(appToken))
	si := slackInteraction{
		channel: "C04SM643KCN",
		client:  *client,
		state:   free,
	}
	si.sendInitialMessage()

	socketClient := socketmode.New(
		client,
		socketmode.OptionDebug(true),
		socketmode.OptionLog(log.New(os.Stdout, "socketmode: ", log.Lshortfile|log.LstdFlags)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func(ctx context.Context, client *slack.Client, socketClient *socketmode.Client) {
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-socketClient.Events:
				switch event.Type {
				case socketmode.EventTypeInteractive:
					callback, ok := event.Data.(slack.InteractionCallback)
					if !ok {
						log.Printf("Could not type cast the event to the MessageEvent: %v\n", event)
						continue
					}
					si.handleInteraction(callback)
					socketClient.Ack(*event.Request)
				default:
					log.Printf("Unexpected event type received: '%v'\n", event.Type)
				}
			}
		}
	}(ctx, client, socketClient)

	socketClient.Run()
}

func (si *slackInteraction) buildAttachment() slack.Attachment {
	text := ""
	actions := []slack.AttachmentAction{}
	switch si.state {
	case free:
		text = "Free to take!"
		actions = []slack.AttachmentAction{
			{
				Name:  take,
				Text:  "Take Staging",
				Type:  "button",
				Value: take,
				Style: buttonPrimary,
			},
		}
	case taken:
		text = fmt.Sprintf("Taken by <@%s>\n", si.currentHolder)
		if len(si.queue) > 0 {
			formattedNames := make([]string, len(si.queue))
			for i, name := range si.queue {
				formattedNames[i] = fmt.Sprintf("<@%s>", name)
			}
			text += "Queue: " + strings.Join(formattedNames, ", ")
		}
		actions = []slack.AttachmentAction{
			{
				Name:  release,
				Text:  "Give up",
				Type:  "button",
				Value: release,
				Style: buttonPrimary,
			},
			{
				Name:  getOnQueue,
				Text:  "Get on Queue",
				Type:  "button",
				Value: getOnQueue,
				Style: buttonPrimaryText,
			},
			{
				Name:  queueNext,
				Text:  "Get in Front of Queue",
				Type:  "button",
				Style: buttonDangerText,
			},
			{
				Name:  override,
				Text:  "Override and take now",
				Type:  "button",
				Value: override,
				Style: buttonDanger,
			},
		}
	}

	return slack.Attachment{
		Text:       text,
		CallbackID: "staging",
		Actions:    actions,
	}
}

func (si *slackInteraction) handleInteraction(callback slack.InteractionCallback) error {
	var updateAttachment *slack.Attachment
	switch callback.Type {
	case slack.InteractionTypeInteractionMessage:
		actions := callback.ActionCallback.AttachmentActions
		action := actions[0]
		user := callback.User.ID
		switch action.Name {
		case take:
			if si.state != taken {
				si.state = taken
				si.currentHolder = user
			}
			updateAttachment = &slack.Attachment{
				Text: fmt.Sprintf("<@%s> took staging.", si.currentHolder),
			}
			si.timer = time.AfterFunc(time.Minute/2, func() {
				si.sendReminder()
			})
		case release:
			if si.currentHolder == user {
				if len(si.queue) > 0 {
					si.currentHolder = si.queue[0]
					si.queue = si.queue[1:]
					si.sendDM(si.currentHolder, slack.MsgOptionAttachments(slack.Attachment{
						Text: "You have staging now",
					}))
				} else {
					si.state = free
					si.currentHolder = ""
				}
				updateAttachment = &slack.Attachment{
					Text: fmt.Sprintf("<@%s> released staging.", user),
				}
				si.timer.Stop()
			} else if slices.Contains(si.queue, user) {
				si.removeUserFromQueue(user)
				updateAttachment = &slack.Attachment{
					Text: fmt.Sprintf("<@%s> removed from queue.", user),
				}
			}
		case getOnQueue:
			if si.canQueue(user) {
				si.queue = append(si.queue, user)
				updateAttachment = &slack.Attachment{
					Text: fmt.Sprintf("<@%s> added to queue.", user),
				}
			}
		case queueNext:
			if user != si.currentHolder && si.queue[0] != user {
				si.removeUserFromQueue(user)
				si.queue = append([]string{user}, si.queue...)
				updateAttachment = &slack.Attachment{
					Text: fmt.Sprintf("<@%s> moved to front of queue.", user),
				}
			}
		case override:
			if si.currentHolder != user {
				si.sendDM(si.currentHolder, slack.MsgOptionAttachments(slack.Attachment{
					Text: fmt.Sprintf("user <@%s> has stolen staging from you", user),
				}))
				si.state = taken
				si.currentHolder = user
				si.removeUserFromQueue(user)
				updateAttachment = &slack.Attachment{
					Text: fmt.Sprintf("<@%s> took staging.", user),
				}
			}
		default:
			return errors.New("invalid interaction")
		}
	default:
		return errors.New("unsupported interaction type")
	}
	if updateAttachment != nil {
		err := si.updateStatus(updateAttachment)
		if err != nil {
			return err
		}
	}
	return nil
}

func (si *slackInteraction) canQueue(user string) bool {
	return user != si.currentHolder && !slices.Contains(si.queue, user)

}

func (si *slackInteraction) removeUserFromQueue(user string) {
	idx := slices.Index(si.queue, user)
	if idx > -1 {
		si.queue = append(si.queue[:idx], si.queue[idx+1:]...)
	}
}

func (si *slackInteraction) sendReminder() {
	if si.state == taken && len(si.queue) > 0 {
		si.sendDM(si.currentHolder, slack.MsgOptionAttachments(slack.Attachment{
			Text: "You have had staging for 30 seconds.\n If you are done, please release staging.\n",
		}))
	}
}

func (si *slackInteraction) updateStatus(updateAttachment *slack.Attachment) error {
	err := si.updateMessage(slack.MsgOptionAttachments(*updateAttachment))
	if err != nil {
		return err
	}
	options := []slack.MsgOption{
		slack.MsgOptionAttachments(
			si.buildAttachment(),
		),
	}
	return si.sendMessage(options...)
}

func (si *slackInteraction) sendInitialMessage() error {
	options := []slack.MsgOption{
		slack.MsgOptionAttachments(si.buildAttachment()),
	}
	return si.sendMessage(options...)
}

func (si *slackInteraction) updateMessage(options ...slack.MsgOption) error {
	_, _, _, err := si.client.UpdateMessage(si.channel, si.ts, options...)
	if err != nil {
		return err
	}
	return nil

}

func (si *slackInteraction) sendMessage(options ...slack.MsgOption) error {
	_, ts, err := si.client.PostMessage(si.channel, options...)
	if err != nil {
		return err
	}
	si.ts = ts
	return nil
}

func (si *slackInteraction) sendDM(user string, options ...slack.MsgOption) error {
	_, _, err := si.client.PostMessage(user, options...)
	if err != nil {
		return err
	}
	return nil
}
