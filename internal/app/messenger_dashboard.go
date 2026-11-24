package app

import (
	"context"
	"errors"
)

// DashboardMessenger is the "no WhatsApp provider" messenger. Alerts are not
// sent anywhere: the Sender writes each one to the notifications table (see
// Store.deliverNotification), so there is no outbound network and no phone
// number is needed for delivery. Login links are never sent either; the
// operator creates them with `webwatcher adduser` or `webwatcher loginlink`.
type DashboardMessenger struct{}

func (*DashboardMessenger) Name() string { return "dashboard" }

// Send always fails: nothing may be sent through the dashboard messenger.
func (*DashboardMessenger) Send(context.Context, Message) error {
	return errors.New("the dashboard messenger sends nothing; alerts are stored as notifications")
}
