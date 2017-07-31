// Copyright 2017 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bytes"
	"database/sql"
	"encoding/gob"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"fmt"
	"github.com/lib/pq"
	"github.com/satori/go.uuid"
	"go.uber.org/zap"
)

type scanner interface {
	Scan(dest ...interface{}) error
}

type groupCursor struct {
	Primary   interface{}
	Secondary int64
	GroupID   []byte
}

func (p *pipeline) groupCreate(logger *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupsCreate()

	if len(e.Groups) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.Groups) > 1 {
		logger.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.Groups[0]
	if g.Name == "" {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group name is mandatory."))
		return
	}

	var group *Group

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not create group", zap.Error(err))
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Could not create group"))
		return
	}

	defer func() {
		if err != nil {
			logger.Error("Could not create group", zap.Error(err))
			if tx != nil {
				txErr := tx.Rollback()
				if txErr != nil {
					logger.Error("Could not rollback transaction", zap.Error(txErr))
				}
			}
			if strings.HasSuffix(err.Error(), "violates unique constraint \"groups_name_key\"") {
				session.Send(ErrorMessage(envelope.CollationId, GROUP_NAME_INUSE, "Name is in use"))
			} else {
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not create group"))
			}
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not create group"))
			} else {
				logger.Info("Created new group", zap.String("name", group.Name))
				session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Groups{&TGroups{Groups: []*Group{group}}}})
			}
		}
	}()

	state := 0
	if g.Private {
		state = 1
	}

	columns := make([]string, 0)
	params := make([]string, 0)
	values := make([]interface{}, 5)

	updatedAt := nowMs()

	values[0] = uuid.NewV4().Bytes()
	values[1] = session.userID.Bytes()
	values[2] = g.Name
	values[3] = state
	values[4] = updatedAt

	if g.Description != "" {
		columns = append(columns, "description")
		params = append(params, "$"+strconv.Itoa(len(values)+1))
		values = append(values, g.Description)
	}

	if g.AvatarUrl != "" {
		columns = append(columns, "avatar_url")
		params = append(params, "$"+strconv.Itoa(len(values)+1))
		values = append(values, g.AvatarUrl)
	}

	if g.Lang != "" {
		columns = append(columns, "lang")
		params = append(params, "$"+strconv.Itoa(len(values)+1))
		values = append(values, g.Lang)
	}

	if g.Metadata != nil {
		// Make this `var js interface{}` if we want to allow top-level JSON arrays.
		var maybeJSON map[string]interface{}
		if json.Unmarshal(g.Metadata, &maybeJSON) != nil {
			session.Send(ErrorMessageBadInput(envelope.CollationId, "Metadata must be a valid JSON object"))
			return
		}

		columns = append(columns, "metadata")
		params = append(params, "$"+strconv.Itoa(len(values)+1))
		values = append(values, g.Metadata)
	}

	r := tx.QueryRow(`
INSERT INTO groups (id, creator_id, name, state, count, created_at, updated_at, `+strings.Join(columns, ", ")+")"+`
VALUES ($1, $2, $3, $4, 1, $5, $5, `+strings.Join(params, ",")+")"+`
RETURNING id, creator_id, name, description, avatar_url, lang, utc_offset_ms, metadata, state, count, created_at, updated_at
`, values...)

	group, err = extractGroup(r)
	if err != nil {
		return
	}

	res, err := tx.Exec(`
INSERT INTO group_edge (source_id, position, updated_at, destination_id, state)
VALUES ($1, $2, $2, $3, 0), ($3, $2, $2, $1, 0)`,
		group.Id, updatedAt, session.userID.Bytes())

	if err != nil {
		return
	}

	rowAffected, err := res.RowsAffected()
	if err != nil {
		return
	}
	if rowAffected == 0 {
		err = errors.New("Could not insert into group_edge table")
		return
	}
}

func (p *pipeline) groupUpdate(l *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupsUpdate()

	if len(e.Groups) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.Groups) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	// Extract the first update.
	g := e.Groups[0]

	// Validate input.
	_, err := uuid.FromBytes(g.GroupId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid."))
		return
	}

	// Make this `var js interface{}` if we want to allow top-level JSON arrays.
	var maybeJSON map[string]interface{}
	if len(g.Metadata) != 0 && json.Unmarshal(g.Metadata, &maybeJSON) != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Metadata must be a valid JSON object"))
		return
	}

	code, err := GroupsUpdate(l, p.db, session.userID, []*TGroupsUpdate_GroupUpdate{g})
	if err != nil {
		session.Send(ErrorMessage(envelope.CollationId, code, err.Error()))
		return
	}

	session.Send(&Envelope{CollationId: envelope.CollationId})
}

func (p *pipeline) groupRemove(l *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupsRemove()

	if len(e.GroupIds) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.GroupIds) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.GroupIds[0]
	//TODO kick all users out
	groupID, err := uuid.FromBytes(g)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid."))
		return
	}

	logger := l.With(zap.String("group_id", groupID.String()))
	failureReason := "Failed to remove group"

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not remove group", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
		return
	}
	defer func() {
		if err != nil {
			logger.Error("Could not remove group", zap.Error(err))
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}
			session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
			} else {
				logger.Info("Removed group")
				session.Send(&Envelope{CollationId: envelope.CollationId})
			}
		}
	}()

	res, err := tx.Exec(`
DELETE FROM groups
WHERE
	id = $1
AND
	EXISTS (SELECT source_id FROM group_edge WHERE source_id = $1 AND destination_id = $2 AND state = 0)
	`, groupID.Bytes(), session.userID.Bytes())

	if err != nil {
		return
	}

	rowAffected, err := res.RowsAffected()
	if err != nil {
		return
	}
	if rowAffected == 0 {
		err = errors.New("Could not remove group. User may not be group admin or group may not exist")
		failureReason = "Could not remove group. Make sure you are a group admin and group exists"
		return
	}

	_, err = tx.Exec("DELETE FROM group_edge WHERE source_id = $1 OR destination_id = $1", groupID.Bytes())
}

func (p *pipeline) groupsFetch(logger *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupsFetch()

	if len(e.Groups) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	}

	statements := []string{}
	params := []interface{}{}

	for _, g := range e.Groups {
		switch g.Id.(type) {
		case *TGroupsFetch_GroupFetch_GroupId:
			groupID, err := uuid.FromBytes(g.GetGroupId())
			if err == nil {
				params = append(params, groupID.Bytes())
				statements = append(statements, "id = $"+strconv.Itoa(len(params)))
			} else {
				session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is invalid"))
				return
			}
		case *TGroupsFetch_GroupFetch_Name:
			params = append(params, g.GetName())
			statements = append(statements, "name = $"+strconv.Itoa(len(params)))
		case nil:
			session.Send(ErrorMessageBadInput(envelope.CollationId, "A fetch identifier is required"))
			return
		default:
			session.Send(ErrorMessageBadInput(envelope.CollationId, "Unknown fetch identifier"))
			return
		}
	}

	if len(statements) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "One or more fetch set values are required"))
		return
	}

	rows, err := p.db.Query(
		`SELECT id, creator_id, name, description, avatar_url, lang, utc_offset_ms, metadata, state, count, created_at, updated_at
FROM groups WHERE disabled_at = 0 AND ( `+strings.Join(statements, " OR ")+" )",
		params...)
	if err != nil {
		logger.Error("Could not get groups", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not get groups"))
		return
	}
	defer rows.Close()

	groups := make([]*Group, 0)
	for rows.Next() {
		group, err := extractGroup(rows)
		if err != nil {
			logger.Error("Could not get groups", zap.Error(err))
			session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not get groups"))
			return
		}
		groups = append(groups, group)
	}

	session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Groups{Groups: &TGroups{Groups: groups}}})
}

func (p *pipeline) groupsList(logger *zap.Logger, session *session, envelope *Envelope) {
	incoming := envelope.GetGroupsList()
	params := make([]interface{}, 0)

	limit := incoming.PageLimit
	if limit == 0 {
		limit = 10
	} else if limit < 10 || limit > 100 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Page limit must be between 10 and 100"))
		return
	}

	foundCursor := false
	paramNumber := 1
	if incoming.Cursor != nil {
		var c groupCursor
		if err := gob.NewDecoder(bytes.NewReader(incoming.Cursor)).Decode(&c); err != nil {
			session.Send(ErrorMessageBadInput(envelope.CollationId, "Invalid cursor data"))
			return
		}

		foundCursor = true
		params = append(params, c.Primary)
		params = append(params, c.Secondary)
		params = append(params, c.GroupID)
		paramNumber = len(params)
	}

	orderBy := "DESC"
	comparison := "<"
	if incoming.OrderByAsc {
		orderBy = "ASC"
		comparison = ">"
	}

	cursorQuery := ""
	filterQuery := ""
	if incoming.GetLang() != "" {
		if foundCursor {
			cursorQuery = "(lang, count, id) " + comparison + " ($1, $2, $3) AND"
		}
		filterQuery = "lang >= $" + strconv.Itoa(paramNumber) + " AND"
		params = append(params, incoming.GetLang())
	} else if incoming.GetCreatedAt() != 0 {
		if foundCursor {
			cursorQuery = "(created_at, count, id) " + comparison + " ($1, $2, $3) AND"
		}
		filterQuery = "created_at >= $" + strconv.Itoa(paramNumber) + " AND"
		params = append(params, incoming.GetCreatedAt())
	} else if incoming.GetCount() != 0 {
		if foundCursor {
			cursorQuery = "(count, updated_at, id) " + comparison + " ($1, $2, $3) AND"
		}
		filterQuery = "count <= $" + strconv.Itoa(paramNumber) + " AND"
		params = append(params, incoming.GetCount())
	}

	params = append(params, limit+1)
	query := `
SELECT id, creator_id, name, description, avatar_url, lang, utc_offset_ms, metadata, state, count, created_at, updated_at
FROM groups WHERE ` + cursorQuery + " " + filterQuery + " disabled_at = 0" + `
ORDER BY count ` + orderBy + " " + `
LIMIT $` + strconv.Itoa(len(params))

	rows, err := p.db.Query(query, params...)
	if err != nil {
		logger.Error("Could not list groups", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not list groups"))
		return
	}
	defer rows.Close()

	groups := make([]*Group, 0)
	var cursor []byte
	var lastGroup *Group
	for rows.Next() {
		if int64(len(groups)) >= limit {
			cursorBuf := new(bytes.Buffer)
			newCursor := &groupCursor{GroupID: lastGroup.Id}
			if incoming.GetLang() != "" {
				newCursor.Primary = lastGroup.Lang
				newCursor.Secondary = lastGroup.Count
			} else if incoming.GetCreatedAt() != 0 {
				newCursor.Primary = lastGroup.CreatedAt
				newCursor.Secondary = lastGroup.Count
			} else {
				newCursor.Primary = lastGroup.Count
				newCursor.Secondary = lastGroup.UpdatedAt
			}
			if gob.NewEncoder(cursorBuf).Encode(newCursor); err != nil {
				logger.Error("Could not create group list cursor", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not list groups"))
				return
			}
			cursor = cursorBuf.Bytes()
			break
		}
		lastGroup, err = extractGroup(rows)
		if err != nil {
			logger.Error("Could not list groups", zap.Error(err))
			session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not list groups"))
			return
		}
		groups = append(groups, lastGroup)
	}

	session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_Groups{Groups: &TGroups{
		Groups: groups,
		Cursor: cursor,
	}}})
}

func (p *pipeline) groupsSelfList(logger *zap.Logger, session *session, envelope *Envelope) {
	groups, code, err := GroupsSelfList(logger, p.db, session.userID, session.userID)
	if err != nil {
		session.Send(ErrorMessage(envelope.CollationId, code, err.Error()))
		return
	}

	session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_GroupsSelf{GroupsSelf: &TGroupsSelf{GroupsSelf: groups}}})
}

func (p *pipeline) groupUsersList(logger *zap.Logger, session *session, envelope *Envelope) {
	g := envelope.GetGroupUsersList()

	groupID, err := uuid.FromBytes(g.GroupId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid"))
		return
	}

	users, code, err := GroupUsersList(logger, p.db, session.userID, groupID)
	if err != nil {
		session.Send(ErrorMessage(envelope.CollationId, code, err.Error()))
		return
	}

	session.Send(&Envelope{CollationId: envelope.CollationId, Payload: &Envelope_GroupUsers{GroupUsers: &TGroupUsers{Users: users}}})
}

func (p *pipeline) groupJoin(l *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupsJoin()

	if len(e.GroupIds) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.GroupIds) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.GroupIds[0]
	groupID, err := uuid.FromBytes(g)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid."))
		return
	}

	logger := l.With(zap.String("group_id", groupID.String()))

	ts := nowMs()

	// Group admin user IDs to notify there's a new user join request, if the group is private.
	var groupName sql.NullString
	privateGroup := false
	adminUserIDs := make([][]byte, 0)

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not add user to group", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not add user to group"))
		return
	}
	defer func() {
		if err != nil {
			logger.Error("Could not join group", zap.Error(err))
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not join group"))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not join group"))
			} else {
				logger.Info("User joined group")
				session.Send(&Envelope{CollationId: envelope.CollationId})

				if !privateGroup {
					// If the user was added directly.
					err = p.storeAndDeliverMessage(logger, session, &TopicId{Id: &TopicId_GroupId{GroupId: groupID.Bytes()}}, 1, []byte("{}"))
					if err != nil {
						logger.Error("Error handling group user join notification topic message", zap.Error(err))
					}
				} else if len(adminUserIDs) != 0 {
					// If the user has requested to join and there are admins to notify.
					handle := session.handle.Load()
					name := groupName.String
					content, err := json.Marshal(map[string]string{"handle": handle, "name": name})
					if err != nil {
						logger.Warn("Failed to send group join request notification", zap.Error(err))
						return
					}
					subject := fmt.Sprintf("%v wants to join your group %v", handle, name)
					userID := session.userID.Bytes()
					expiresAt := ts + p.notificationService.expiryMs

					notifications := make([]*NNotification, len(adminUserIDs))
					for i, adminUserID := range adminUserIDs {
						notifications[i] = &NNotification{
							Id:         uuid.NewV4().Bytes(),
							UserID:     adminUserID,
							Subject:    subject,
							Content:    content,
							Code:       NOTIFICATION_GROUP_JOIN_REQUEST,
							SenderID:   userID,
							CreatedAt:  ts,
							ExpiresAt:  expiresAt,
							Persistent: true,
						}
					}

					err = p.notificationService.NotificationSend(notifications)
					if err != nil {
						logger.Warn("Failed to send group join request notification", zap.Error(err))
					}
				}
			}
		}
	}()

	var groupState sql.NullInt64
	err = tx.QueryRow("SELECT state, name FROM groups WHERE id = $1 AND disabled_at = 0", groupID.Bytes()).Scan(&groupState, &groupName)
	if err != nil {
		return
	}

	userState := 1
	if groupState.Int64 == 1 {
		privateGroup = true
		userState = 2
	}

	res, err := tx.Exec(`
INSERT INTO group_edge (source_id, position, updated_at, destination_id, state)
VALUES ($1, $2, $2, $3, $4), ($3, $2, $2, $1, $4)`,
		groupID.Bytes(), ts, session.userID.Bytes(), userState)

	if err != nil {
		return
	}

	if affectedRows, _ := res.RowsAffected(); affectedRows == 0 {
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not accept group join envelope. Group may not exists with the given ID"))
		return
	}

	// If the group is not private and the user joined directly, increase the group count.
	if !privateGroup {
		_, err = tx.Exec("UPDATE groups SET count = count + 1, updated_at = $2 WHERE id = $1", groupID.Bytes(), ts)
	}
	if err != nil {
		return
	}

	// If group is private, look up admin user IDs to notify about a new user requesting to join.
	if privateGroup {
		rows, e := tx.Query("SELECT destination_id FROM group_edge WHERE source_id = $1 AND state = 0", groupID.Bytes())
		if e != nil {
			logger.Warn("Failed to send group join request notification", zap.Error(e))
			return
		}
		defer rows.Close()

		for rows.Next() {
			var adminUserID []byte
			e = rows.Scan(&adminUserID)
			if e != nil {
				logger.Warn("Failed to send group join request notification", zap.Error(e))
				return
			}
			adminUserIDs = append(adminUserIDs, adminUserID)
		}
	}
}

func (p *pipeline) groupLeave(l *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupsLeave()

	if len(e.GroupIds) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.GroupIds) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.GroupIds[0]
	groupID, err := uuid.FromBytes(g)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid"))
		return
	}

	logger := l.With(zap.String("group_id", groupID.String()))

	code := RUNTIME_EXCEPTION
	failureReason := "Could not leave group"
	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not leave group", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
		return
	}
	defer func() {
		if err != nil {
			logger.Error("Could not leave group", zap.Error(err))
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(ErrorMessage(envelope.CollationId, code, failureReason))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
			} else {
				logger.Info("User left group")
				session.Send(&Envelope{CollationId: envelope.CollationId})

				err = p.storeAndDeliverMessage(logger, session, &TopicId{Id: &TopicId_GroupId{GroupId: groupID.Bytes()}}, 3, []byte("{}"))
				if err != nil {
					logger.Error("Error handling group user leave notification topic message", zap.Error(err))
				}
			}
		}
	}()

	// first remove any invitation from user
	// and if this wasn't an invitation then
	// look to see if the user is an admin
	// and remove the user from group and update group count
	res, err := tx.Exec(`
DELETE FROM group_edge
WHERE
	(source_id = $1 AND destination_id = $2 AND state = 2)
OR
	(source_id = $2 AND destination_id = $1 AND state = 2)`,
		groupID.Bytes(), session.userID.Bytes())

	if err != nil {
		return
	}

	if count, _ := res.RowsAffected(); count > 0 {
		logger.Debug("Group invitation removed.")
		return
	}

	var adminCount sql.NullInt64
	err = tx.QueryRow(`
SELECT COUNT(source_id)	FROM group_edge
WHERE
	source_id = $1 AND state = 0
AND
	EXISTS (SELECT id FROM groups WHERE id = $1 AND disabled_at = 0)
AND
	EXISTS (SELECT source_id FROM group_edge WHERE source_id = $1 AND destination_id = $2 AND state = 0)`,
		groupID.Bytes(), session.userID.Bytes()).Scan(&adminCount)

	if err != nil {
		return
	}

	if adminCount.Int64 == 1 {
		code = GROUP_LAST_ADMIN
		failureReason = "Cannot leave group when you are the last group admin"
		err = errors.New("Cannot leave group when you are the last group admin")
		return
	}

	res, err = tx.Exec(`
DELETE FROM group_edge
WHERE
	(source_id = $1 AND destination_id = $2)
OR
	(source_id = $2 AND destination_id = $1)`,
		groupID.Bytes(), session.userID.Bytes())

	if err != nil {
		return
	}

	if count, _ := res.RowsAffected(); count == 0 {
		failureReason = "Cannot leave group - Make sure you are part of the group or group exists"
		err = errors.New("Cannot leave group - Make sure you are part of the group or group exists")
		return
	}

	_, err = tx.Exec(`UPDATE groups SET count = count - 1, updated_at = $1 WHERE id = $2`, nowMs(), groupID.Bytes())
	if err != nil {
		return
	}
}

func (p *pipeline) groupUserAdd(l *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupUsersAdd()

	if len(e.GroupUsers) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.GroupUsers) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.GroupUsers[0]
	groupID, err := uuid.FromBytes(g.GroupId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid"))
		return
	}

	userID, err := uuid.FromBytes(g.UserId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User ID is not valid"))
		return
	}

	logger := l.With(zap.String("group_id", groupID.String()), zap.String("user_id", userID.String()))
	ts := nowMs()
	var handle string
	var name string

	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not add user to group", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not add user to group"))
		return
	}
	defer func() {
		if err != nil {
			if _, ok := err.(*pq.Error); ok {
				logger.Error("Could not add user to group", zap.Error(err))
			} else {
				logger.Warn("Could not add user to group", zap.Error(err))
			}
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not add user to group"))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not add user to group"))
			} else {
				logger.Info("Added user to the group")
				session.Send(&Envelope{CollationId: envelope.CollationId})

				data, _ := json.Marshal(map[string]string{"user_id": userID.String(), "handle": handle})
				err = p.storeAndDeliverMessage(logger, session, &TopicId{Id: &TopicId_GroupId{GroupId: groupID.Bytes()}}, 2, data)
				if err != nil {
					logger.Error("Error handling group user added notification topic message", zap.Error(err))
					return
				}

				adminHandle := session.handle.Load()
				content, err := json.Marshal(map[string]string{"handle": adminHandle, "name": name})
				if err != nil {
					logger.Warn("Failed to send group add notification", zap.Error(err))
					return
				}
				err = p.notificationService.NotificationSend([]*NNotification{
					&NNotification{
						Id:         uuid.NewV4().Bytes(),
						UserID:     userID.Bytes(),
						Subject:    fmt.Sprintf("%v has added you to group %v", adminHandle, name),
						Content:    content,
						Code:       NOTIFICATION_GROUP_ADD,
						SenderID:   session.userID.Bytes(),
						CreatedAt:  ts,
						ExpiresAt:  ts + p.notificationService.expiryMs,
						Persistent: true,
					},
				})
				if err != nil {
					logger.Warn("Failed to send group add notification", zap.Error(err))
				}
			}
		}
	}()

	// Look up the user being added.
	err = tx.QueryRow("SELECT handle FROM users WHERE id = $1 AND disabled_at = 0", userID.Bytes()).Scan(&handle)
	if err != nil {
		return
	}

	// Look up the name of the group.
	err = tx.QueryRow("SELECT name FROM groups WHERE id = $1", groupID.Bytes()).Scan(&name)
	if err != nil {
		return
	}

	res, err := tx.Exec(`
INSERT INTO group_edge (source_id, position, updated_at, destination_id, state)
SELECT data.id, data.position, data.updated_at, data.destination, data.state
FROM (
  SELECT $1::BYTEA AS id, $2::INT AS position, $2::INT AS updated_at, $3::BYTEA AS destination, 1 AS state
  UNION ALL
  SELECT $3::BYTEA AS id, $2::INT AS position, $2::INT AS updated_at, $1::BYTEA AS destination, 1 AS state
) AS data
WHERE
  EXISTS (SELECT source_id FROM group_edge WHERE source_id = $1::BYTEA AND destination_id = $4::BYTEA AND state = 0)
AND
  EXISTS (SELECT id FROM groups WHERE id = $1::BYTEA AND disabled_at = 0)
ON CONFLICT (source_id, destination_id)
DO UPDATE SET state = 1, updated_at = $2::INT`,
		groupID.Bytes(), ts, userID.Bytes(), session.userID.Bytes())

	if err != nil {
		return
	}

	if affectedRows, _ := res.RowsAffected(); affectedRows == 0 {
		err = errors.New("Could not add user to group. Group may not exists or you may not be group admin")
		return
	}

	_, err = tx.Exec(`UPDATE groups SET count = count + 1, updated_at = $1 WHERE id = $2`, nowMs(), groupID.Bytes())
	if err != nil {
		return
	}
}

func (p *pipeline) groupUserKick(l *zap.Logger, session *session, envelope *Envelope) {
	// TODO Force kick the user out.
	e := envelope.GetGroupUsersKick()

	if len(e.GroupUsers) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.GroupUsers) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.GroupUsers[0]

	groupID, err := uuid.FromBytes(g.GroupId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid"))
		return
	}

	userID, err := uuid.FromBytes(g.UserId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User ID is not valid"))
		return
	}

	if userID == session.userID {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "You can't kick yourself from the group"))
		return
	}

	logger := l.With(zap.String("group_id", groupID.String()), zap.String("user_id", userID.String()))
	var handle string

	failureReason := "Could not kick user from group"
	tx, err := p.db.Begin()
	if err != nil {
		logger.Error("Could not kick user from group", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
		return
	}
	defer func() {
		if err != nil {
			if _, ok := err.(*pq.Error); ok {
				logger.Error("Could not kick user from group", zap.Error(err))
			} else {
				logger.Warn("Could not kick user from group", zap.Error(err))
			}
			err = tx.Rollback()
			if err != nil {
				logger.Error("Could not rollback transaction", zap.Error(err))
			}

			session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
		} else {
			err = tx.Commit()
			if err != nil {
				logger.Error("Could not commit transaction", zap.Error(err))
				session.Send(ErrorMessageRuntimeException(envelope.CollationId, failureReason))
			} else {
				logger.Info("Kicked user from group")
				session.Send(&Envelope{CollationId: envelope.CollationId})

				data, _ := json.Marshal(map[string]string{"user_id": userID.String(), "handle": handle})
				err = p.storeAndDeliverMessage(logger, session, &TopicId{Id: &TopicId_GroupId{GroupId: groupID.Bytes()}}, 4, data)
				if err != nil {
					logger.Error("Error handling group user kicked notification topic message", zap.Error(err))
				}
			}
		}
	}()

	// Check the user's group_edge state. If it's a pending join request being rejected then no need to decrement the group count.
	var userState int64
	err = tx.QueryRow("SELECT state FROM group_edge WHERE source_id = $1 AND destination_id = $2", groupID.Bytes(), userID.Bytes()).Scan(&userState)

	res, err := tx.Exec(`
DELETE FROM group_edge
WHERE
	EXISTS (SELECT source_id FROM group_edge WHERE source_id = $1 AND destination_id = $3 AND state = 0)
AND
	EXISTS (SELECT id FROM groups WHERE id = $1 AND disabled_at = 0)
AND
	(
		(source_id = $1 AND destination_id = $2)
	OR
		(source_id = $2 AND destination_id = $1)
	)`, groupID.Bytes(), userID.Bytes(), session.userID.Bytes())

	if err != nil {
		return
	}

	if count, _ := res.RowsAffected(); count == 0 {
		failureReason = "Cannot kick from group - Make sure user is part of the group and is admin or group exists"
		err = errors.New("Cannot kick from group - Make sure user is part of the group and is admin or group exists")
		return
	}

	// Join requests aren't reflected in group count.
	if userState != 2 {
		_, err = tx.Exec(`UPDATE groups SET count = count - 1, updated_at = $1 WHERE id = $2`, nowMs(), groupID.Bytes())
		if err != nil {
			return
		}
	}

	// Look up the user being kicked. Allow kicking disabled users.
	err = tx.QueryRow("SELECT handle FROM users WHERE id = $1", userID.Bytes()).Scan(&handle)
	if err != nil {
		return
	}
}

func (p *pipeline) groupUserPromote(l *zap.Logger, session *session, envelope *Envelope) {
	e := envelope.GetGroupUsersPromote()

	if len(e.GroupUsers) == 0 {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "At least one item must be present"))
		return
	} else if len(e.GroupUsers) > 1 {
		l.Warn("There are more than one item passed to the request - only processing the first item.")
	}

	g := e.GroupUsers[0]
	groupID, err := uuid.FromBytes(g.GroupId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "Group ID is not valid"))
		return
	}

	userID, err := uuid.FromBytes(g.UserId)
	if err != nil {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "User ID is not valid"))
		return
	}

	if userID == session.userID {
		session.Send(ErrorMessageBadInput(envelope.CollationId, "You can't promote yourself"))
		return
	}

	logger := l.With(zap.String("group_id", groupID.String()), zap.String("user_id", userID.String()))

	res, err := p.db.Exec(`
UPDATE group_edge SET state = 0, updated_at = $4
WHERE
	EXISTS (SELECT source_id FROM group_edge WHERE source_id = $1 AND destination_id = $3 AND state = 0)
AND
	EXISTS (SELECT id FROM groups WHERE id = $1 AND disabled_at = 0)
AND
	(
		(source_id = $1 AND destination_id = $2)
	OR
		(source_id = $2 AND destination_id = $1)
	)`, groupID.Bytes(), userID.Bytes(), session.userID.Bytes(), nowMs())

	if err != nil {
		logger.Warn("Could not promote user", zap.Error(err))
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not promote user"))
		return
	}

	if count, _ := res.RowsAffected(); count == 0 {
		logger.Warn("Could not promote user - Make sure user is part of the group or group exists")
		session.Send(ErrorMessageRuntimeException(envelope.CollationId, "Could not promote user - Make sure user is part of the group or group exists"))
		return
	}

	// Look up the user being promoted. Allow promoting disabled users as long as they're still part of the group.
	var handle string
	err = p.db.QueryRow("SELECT handle FROM users WHERE id = $1", userID.Bytes()).Scan(&handle)
	if err != nil {
		return
	}

	data, _ := json.Marshal(map[string]string{"user_id": userID.String(), "handle": handle})
	err = p.storeAndDeliverMessage(logger, session, &TopicId{Id: &TopicId_GroupId{GroupId: groupID.Bytes()}}, 5, data)
	if err != nil {
		logger.Error("Error handling group user promoted notification topic message", zap.Error(err))
	}

	session.Send(&Envelope{CollationId: envelope.CollationId})
}
