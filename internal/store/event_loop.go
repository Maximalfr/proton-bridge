// Copyright (c) 2020 Proton Technologies AG
//
// This file is part of ProtonMail Bridge.
//
// ProtonMail Bridge is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// ProtonMail Bridge is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with ProtonMail Bridge.  If not, see <https://www.gnu.org/licenses/>.

package store

import (
	"time"

	bridgeEvents "github.com/ProtonMail/proton-bridge/internal/events"
	"github.com/ProtonMail/proton-bridge/pkg/listener"
	"github.com/ProtonMail/proton-bridge/pkg/pmapi"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const pollInterval = 30 * time.Second

type eventLoop struct {
	cache          *Cache
	currentEventID string
	pollCh         chan chan struct{}
	stopCh         chan struct{}
	notifyStopCh   chan struct{}
	isRunning      bool
	hasInternet    bool

	log *logrus.Entry

	store     *Store
	apiClient PMAPIProvider
	user      BridgeUser
	events    listener.Listener
}

func newEventLoop(cache *Cache, store *Store, api PMAPIProvider, user BridgeUser, events listener.Listener) *eventLoop {
	eventLog := log.WithField("userID", user.ID())
	eventLog.Trace("Creating new event loop")

	return &eventLoop{
		cache:          cache,
		currentEventID: cache.getEventID(user.ID()),
		pollCh:         make(chan chan struct{}),
		isRunning:      false,

		log: eventLog,

		store:     store,
		apiClient: api,
		user:      user,
		events:    events,
	}
}

func (loop *eventLoop) IsRunning() bool {
	return loop.isRunning
}

func (loop *eventLoop) setFirstEventID() (err error) {
	loop.log.Trace("Setting first event ID")

	event, err := loop.apiClient.GetEvent("")
	if err != nil {
		loop.log.WithError(err).Error("Could not get latest event ID")
		return
	}

	loop.currentEventID = event.EventID

	if err = loop.cache.setEventID(loop.user.ID(), loop.currentEventID); err != nil {
		loop.log.WithError(err).Error("Could not set latest event ID in user cache")
		return
	}

	return
}

// pollNow starts polling events right away and waits till the events are
// processed so we are sure updates are propagated to the database.
func (loop *eventLoop) pollNow() {
	eventProcessedCh := make(chan struct{})
	loop.pollCh <- eventProcessedCh
	<-eventProcessedCh
	close(eventProcessedCh)
}

func (loop *eventLoop) stop() {
	if loop.isRunning {
		loop.isRunning = false
		close(loop.stopCh)

		select {
		case <-loop.notifyStopCh:
			loop.log.Info("Event loop was stopped")
		case <-time.After(1 * time.Second):
			loop.log.Warn("Timed out waiting for event loop to stop")
		}
	}
}

func (loop *eventLoop) start() { // nolint[funlen]
	if loop.isRunning {
		return
	}
	defer func() {
		loop.isRunning = false
	}()
	loop.stopCh = make(chan struct{})
	loop.notifyStopCh = make(chan struct{})
	loop.isRunning = true

	events := make(chan *pmapi.Event)
	defer close(events)

	loop.log.WithField("lastEventID", loop.currentEventID).Info("Subscribed to events")
	defer func() {
		loop.log.WithField("lastEventID", loop.currentEventID).Info("Subscription stopped")
	}()

	t := time.NewTicker(pollInterval)
	defer t.Stop()

	loop.hasInternet = true

	go loop.pollNow()

	for {
		var eventProcessedCh chan struct{}
		select {
		case <-loop.stopCh:
			close(loop.notifyStopCh)
			return
		case eventProcessedCh = <-loop.pollCh:
		case <-t.C:
		}

		// Before we fetch the first event, check whether this is the first time we've
		// started the event loop, and if so, trigger a full sync.
		// In case internet connection was not available during start, it will be
		// handled anyway when the connection is back here.
		if loop.isBeforeFirstStart() {
			if eventErr := loop.setFirstEventID(); eventErr != nil {
				loop.log.WithError(eventErr).Warn("Could not set initial event ID")
			}
		}

		// If the sync is not finished then a new sync is triggered.
		if !loop.store.isSyncFinished() {
			loop.store.triggerSync()
		}

		more, err := loop.processNextEvent()
		if eventProcessedCh != nil {
			eventProcessedCh <- struct{}{}
		}
		if err != nil {
			loop.log.WithError(err).Error("Cannot process event, stopping event loop")
			// When event loop stops, the only way to start it again is by login.
			// It should stop only when user is logged out but even if there is other
			// serious error, logout is intended action.
			if errLogout := loop.user.Logout(); errLogout != nil {
				loop.log.
					WithError(errLogout).
					Error("Failed to logout user after loop finished with error")
			}
			return
		}

		if more {
			go loop.pollNow()
		}
	}
}

// isBeforeFirstStart returns whether the initial event ID was already set or not.
func (loop *eventLoop) isBeforeFirstStart() bool {
	return loop.currentEventID == ""
}

// processNextEvent saves only successfully processed `eventID` into cache
// (disk). It will filter out in defer all errors except invalid token error.
// Invalid error will be returned and stop the event loop.
func (loop *eventLoop) processNextEvent() (more bool, err error) { // nolint[funlen]
	l := loop.log.WithField("currentEventID", loop.currentEventID)

	// We only want to consider invalid tokens as real errors because all other errors might fix themselves eventually
	// (e.g. no internet, ulimit reached etc.)
	defer func() {
		if errors.Cause(err) == pmapi.ErrAPINotReachable {
			l.Warn("Internet unavailable")
			loop.events.Emit(bridgeEvents.InternetOffEvent, "")
			loop.hasInternet = false
			err = nil
		}

		if err != nil && isFdCloseToULimit() {
			l.Warn("Ulimit reached")
			loop.events.Emit(bridgeEvents.RestartBridgeEvent, "")
			err = nil
		}

		if errors.Cause(err) == pmapi.ErrUpgradeApplication {
			l.Warn("Need to upgrade application")
			loop.events.Emit(bridgeEvents.UpgradeApplicationEvent, "")
			err = nil
		}

		_, errUnauthorized := errors.Cause(err).(*pmapi.ErrUnauthorized)

		// All errors except Invalid Token (which is not possible to recover from) are ignored.
		if err != nil && !errUnauthorized && errors.Cause(err) != pmapi.ErrInvalidToken {
			l.WithError(err).Trace("Error skipped")
			err = nil
		}
	}()

	l.Trace("Polling next event")
	var event *pmapi.Event
	if event, err = loop.apiClient.GetEvent(loop.currentEventID); err != nil {
		return false, errors.Wrap(err, "failed to get event")
	}

	l = l.WithField("newEventID", event.EventID)

	if !loop.hasInternet {
		loop.events.Emit(bridgeEvents.InternetOnEvent, "")
		loop.hasInternet = true
	}

	if err = loop.processEvent(event); err != nil {
		return false, errors.Wrap(err, "failed to process event")
	}

	if loop.currentEventID != event.EventID {
		// In case new event ID cannot be saved to cache, we update it in event loop
		// anyway and continue processing new events to prevent the loop from repeatedly
		// processing the same event.
		// This allows the event loop to continue to function (unless the cache was broken
		// and bridge stopped, in which case it will start from the old event ID anyway).
		loop.currentEventID = event.EventID
		if err = loop.cache.setEventID(loop.user.ID(), event.EventID); err != nil {
			return false, errors.Wrap(err, "failed to save event ID to cache")
		}
	}

	return event.More == 1, err
}

func (loop *eventLoop) processEvent(event *pmapi.Event) (err error) {
	eventLog := loop.log.WithField("event", event.EventID)
	eventLog.Debug("Processing event")

	if (event.Refresh & pmapi.EventRefreshMail) != 0 {
		eventLog.Info("Processing refresh event")
		loop.store.triggerSync()

		return
	}

	if len(event.Addresses) != 0 {
		if err = loop.processAddresses(eventLog, event.Addresses); err != nil {
			return errors.Wrap(err, "failed to process address events")
		}
	}

	if len(event.Labels) != 0 {
		if err = loop.processLabels(eventLog, event.Labels); err != nil {
			return errors.Wrap(err, "failed to process label events")
		}
	}

	if len(event.Messages) != 0 {
		if err = loop.processMessages(eventLog, event.Messages); err != nil {
			return errors.Wrap(err, "failed to process message events")
		}
	}

	// One would expect that every event would contain MessageCount as part of
	// the event.Messages, but this is apparently not the case.
	// MessageCounts are served on an irregular basis, so we should update and
	// compare the counts only when we receive them.
	if len(event.MessageCounts) != 0 {
		if err = loop.processMessageCounts(eventLog, event.MessageCounts); err != nil {
			return errors.Wrap(err, "failed to process message count events")
		}
	}

	if len(event.Notices) != 0 {
		loop.processNotices(eventLog, event.Notices)
	}

	return err
}

func (loop *eventLoop) processAddresses(log *logrus.Entry, addressEvents []*pmapi.EventAddress) (err error) {
	log.Debug("Processing address change event")

	// Get old addresses for comparisons before updating user.
	oldList := loop.apiClient.Addresses()

	if err = loop.user.UpdateUser(); err != nil {
		if logoutErr := loop.user.Logout(); logoutErr != nil {
			log.WithError(logoutErr).Error("Failed to logout user after failed update")
		}
		return errors.Wrap(err, "failed to update user")
	}

	for _, addressEvent := range addressEvents {
		switch addressEvent.Action {
		case pmapi.EventCreate:
			log.WithField("email", addressEvent.Address.Email).Debug("Address was created")
			loop.events.Emit(bridgeEvents.AddressChangedEvent, loop.user.GetPrimaryAddress())

		case pmapi.EventUpdate:
			oldAddress := oldList.ByID(addressEvent.ID)
			if oldAddress == nil {
				log.Warning("Event refers to an address that isn't present")
				continue
			}

			email := oldAddress.Email
			log.WithField("email", email).Debug("Address was updated")
			if addressEvent.Address.Receive != oldAddress.Receive {
				loop.events.Emit(bridgeEvents.AddressChangedLogoutEvent, email)
			}

		case pmapi.EventDelete:
			oldAddress := oldList.ByID(addressEvent.ID)
			if oldAddress == nil {
				log.Warning("Event refers to an address that isn't present")
				continue
			}

			email := oldAddress.Email
			log.WithField("email", email).Debug("Address was deleted")
			loop.user.CloseConnection(email)
			loop.events.Emit(bridgeEvents.AddressChangedLogoutEvent, email)
		}
	}

	if err = loop.store.createOrUpdateAddressInfo(loop.apiClient.Addresses()); err != nil {
		return errors.Wrap(err, "failed to update address IDs in store")
	}

	if err = loop.store.createOrDeleteAddressesEvent(); err != nil {
		return errors.Wrap(err, "failed to create/delete store addresses")
	}

	return nil
}

func (loop *eventLoop) processLabels(eventLog *logrus.Entry, labels []*pmapi.EventLabel) error {
	eventLog.Debug("Processing label change event")

	for _, eventLabel := range labels {
		label := eventLabel.Label
		switch eventLabel.Action {
		case pmapi.EventCreate, pmapi.EventUpdate:
			if err := loop.store.createOrUpdateMailboxEvent(label); err != nil {
				return errors.Wrap(err, "failed to create or update label")
			}
		case pmapi.EventDelete:
			if err := loop.store.deleteMailboxEvent(eventLabel.ID); err != nil {
				return errors.Wrap(err, "failed to delete label")
			}
		}
	}

	return nil
}

func (loop *eventLoop) processMessages(eventLog *logrus.Entry, messages []*pmapi.EventMessage) (err error) {
	eventLog.Debug("Processing message change event")

	for _, message := range messages {
		msgLog := eventLog.WithField("msgID", message.ID)

		switch message.Action {
		case pmapi.EventCreate:
			msgLog.Debug("Processing EventCreate for message")

			if message.Created == nil {
				msgLog.Error("Got EventCreate with nil message")
				break
			}

			if err = loop.store.createOrUpdateMessageEvent(message.Created); err != nil {
				return errors.Wrap(err, "failed to put message into DB")
			}

		case pmapi.EventUpdate, pmapi.EventUpdateFlags:
			msgLog.Debug("Processing EventUpdate(Flags) for message")

			if message.Updated == nil {
				msgLog.Errorf("Got EventUpdate(Flags) with nil message")
				break
			}

			var msg *pmapi.Message
			msg, err = loop.store.getMessageFromDB(message.ID)
			if err == ErrNoSuchAPIID {
				msgLog.WithError(err).Warning("Cannot get message from DB for updating. Trying fetch...")
				msg, err = loop.store.fetchMessage(message.ID)
				// If message does not exist anywhere, update event is probably old and off topic - skip it.
				if err == ErrNoSuchAPIID {
					msgLog.Warn("Skipping message update, because message does not exist nor in local DB or on API")
					continue
				}
			}
			if err != nil {
				return errors.Wrap(err, "failed to get message from DB for updating")
			}

			updateMessage(msgLog, msg, message.Updated)

			if err = loop.store.createOrUpdateMessageEvent(msg); err != nil {
				return errors.Wrap(err, "failed to update message in DB")
			}

		case pmapi.EventDelete:
			msgLog.Debug("Processing EventDelete for message")

			if err = loop.store.deleteMessageEvent(message.ID); err != nil {
				return errors.Wrap(err, "failed to delete message from DB")
			}
		}
	}

	return err
}

func updateMessage(msgLog *logrus.Entry, message *pmapi.Message, updates *pmapi.EventMessageUpdated) { //nolint[funlen]
	msgLog.Debug("Updating message")

	message.Time = updates.Time

	if updates.Subject != nil {
		msgLog.WithField("subject", *updates.Subject).Trace("Updating message value")
		message.Subject = *updates.Subject
	}

	if updates.Sender != nil {
		msgLog.WithField("sender", *updates.Sender).Trace("Updating message value")
		message.Sender = updates.Sender
	}

	if updates.ToList != nil {
		msgLog.WithField("toList", *updates.ToList).Trace("Updating message value")
		message.ToList = *updates.ToList
	}

	if updates.CCList != nil {
		msgLog.WithField("ccList", *updates.CCList).Trace("Updating message value")
		message.CCList = *updates.CCList
	}

	if updates.BCCList != nil {
		msgLog.WithField("bccList", *updates.BCCList).Trace("Updating message value")
		message.BCCList = *updates.BCCList
	}

	if updates.Unread != nil {
		msgLog.WithField("unread", *updates.Unread).Trace("Updating message value")
		message.Unread = *updates.Unread
	}

	if updates.Flags != nil {
		msgLog.WithField("flags", *updates.Flags).Trace("Updating message value")
		message.Flags = *updates.Flags
	}

	if updates.LabelIDs != nil {
		msgLog.WithField("labelIDs", updates.LabelIDs).Trace("Updating message value")
		message.LabelIDs = updates.LabelIDs
	} else {
		for _, added := range updates.LabelIDsAdded {
			hasLabel := false
			for _, l := range message.LabelIDs {
				if added == l {
					hasLabel = true
					break
				}
			}
			if !hasLabel {
				msgLog.WithField("added", added).Trace("Adding label to message")
				message.LabelIDs = append(message.LabelIDs, added)
			}
		}

		labels := []string{}
		for _, l := range message.LabelIDs {
			removeLabel := false
			for _, removed := range updates.LabelIDsRemoved {
				if removed == l {
					removeLabel = true
					break
				}
			}
			if removeLabel {
				msgLog.WithField("label", l).Trace("Removing label from message")
			} else {
				labels = append(labels, l)
			}
		}

		message.LabelIDs = labels
	}
}

func (loop *eventLoop) processMessageCounts(l *logrus.Entry, messageCounts []*pmapi.MessagesCount) error {
	l.WithField("apiCounts", messageCounts).Debug("Processing message count change event")

	isSynced, err := loop.store.isSynced(messageCounts)
	if err != nil {
		return err
	}
	if !isSynced {
		loop.store.triggerSync()
	}

	return nil
}

func (loop *eventLoop) processNotices(l *logrus.Entry, notices []string) {
	l.Debug("Processing notice change event")

	for _, notice := range notices {
		l.Infof("Notice: %q", notice)
		for _, address := range loop.user.GetStoreAddresses() {
			loop.store.imapNotice(address, notice)
		}
	}
}
