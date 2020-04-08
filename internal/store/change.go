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

	"github.com/ProtonMail/proton-bridge/pkg/message"
	"github.com/ProtonMail/proton-bridge/pkg/pmapi"
	imap "github.com/emersion/go-imap"
	imapBackend "github.com/emersion/go-imap/backend"
	"github.com/sirupsen/logrus"
)

// SetIMAPUpdateChannel sets the channel on which imap update messages will be sent. This should be the channel
// on which the imap backend listens for imap updates.
func (store *Store) SetIMAPUpdateChannel(updates chan interface{}) {
	store.log.Debug("Listening for IMAP updates")

	if store.imapUpdates = updates; store.imapUpdates == nil {
		store.log.Error("The IMAP Updates channel is nil")
	}
}

func (store *Store) imapNotice(address, notice string) {
	update := new(imapBackend.StatusUpdate)
	update.Username = address
	update.StatusResp = &imap.StatusResp{
		Type: imap.StatusOk,
		Code: imap.CodeAlert,
		Info: notice,
	}
	store.imapSendUpdate(update)
}

func (store *Store) imapUpdateMessage(address, mailboxName string, uid, sequenceNumber uint32, msg *pmapi.Message) {
	store.log.WithFields(logrus.Fields{
		"address": address,
		"mailbox": mailboxName,
		"seqNum":  sequenceNumber,
		"uid":     uid,
		"flags":   message.GetFlags(msg),
	}).Trace("IDLE update")
	update := new(imapBackend.MessageUpdate)
	update.Username = address
	update.Mailbox = mailboxName
	update.Message = imap.NewMessage(sequenceNumber, []string{imap.FlagsMsgAttr, imap.UidMsgAttr})
	update.Message.Flags = message.GetFlags(msg)
	update.Message.Uid = uid
	store.imapSendUpdate(update)
}

func (store *Store) imapDeleteMessage(address, mailboxName string, sequenceNumber uint32) {
	store.log.WithFields(logrus.Fields{
		"address": address,
		"mailbox": mailboxName,
		"seqNum":  sequenceNumber,
	}).Trace("IDLE delete")
	update := new(imapBackend.ExpungeUpdate)
	update.Username = address
	update.Mailbox = mailboxName
	update.SeqNum = sequenceNumber
	store.imapSendUpdate(update)
}

func (store *Store) imapMailboxStatus(address, mailboxName string, total, unread uint) {
	store.log.WithFields(logrus.Fields{
		"address": address,
		"mailbox": mailboxName,
		"total":   total,
		"unread":  unread,
	}).Trace("IDLE status")
	update := new(imapBackend.MailboxUpdate)
	update.Username = address
	update.Mailbox = mailboxName
	update.MailboxStatus = imap.NewMailboxStatus(mailboxName, []string{imap.MailboxMessages, imap.MailboxUnseen})
	update.MailboxStatus.Messages = uint32(total)
	update.MailboxStatus.Unseen = uint32(unread)
	store.imapSendUpdate(update)
}

func (store *Store) imapSendUpdate(update interface{}) {
	if store.imapUpdates == nil {
		store.log.Trace("IMAP IDLE unavailable")
		return
	}

	select {
	case <-time.After(1 * time.Second):
		store.log.Error("Could not send IMAP update (timeout)")
		return
	case store.imapUpdates <- update:
	}
}