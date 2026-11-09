package apis

// Fork-local addition (issue #106, M7): realtime across replicas.
//
// The subscriptions broker is per process: a client is connected to one
// replica, and a record change made on another replica never passes through its
// hooks. So every replica publishes the record changes it makes on the replica
// bus (LISTEN/NOTIFY), and every other replica runs them through the very same
// broadcast code against its own clients, with the rules and the filter of each
// subscription evaluated on the receiving replica, against that client's auth.
//
//	create, update  the event carries only (collection, record id); the receiver
//	                reads the record and broadcasts it, so it sends the state of
//	                the moment it received the event, like a client that
//	                refetched. Two quick updates may reach a client as the
//	                latest state twice, never as an older state after a newer.
//	delete          the row is gone when the event arrives, and a rule is SQL
//	                that needs the row. The event therefore carries the field
//	                values (not the password hashes and not the encrypted
//	                fields), and the receiver puts the row back inside a
//	                transaction it always rolls back, evaluates the rules there
//	                and broadcasts. Nothing is ever committed, and no hook runs.
//
// Guarantees: at most once (NOTIFY is not stored), see tools/replica. A
// client of replica A gets a change made on replica B typically within a few
// milliseconds. Events that happen while a replica is disconnected from the
// database are lost to its clients, which is why the SDK should refetch after
// a reconnect of the realtime stream, as it should anyway.

import (
	"encoding/json/v2"
	"errors"
	"log/slog"
	"strings"

	"github.com/doodlemania2/claviger/core"
	"github.com/doodlemania2/claviger/tools/hook"
	"github.com/doodlemania2/claviger/tools/replica"
	"github.com/doodlemania2/claviger/tools/subscriptions"
)

const (
	replicaEventRealtime     = "rt"
	replicaEventRealtimeAuth = "rt.auth"
	replicaEventOAuth2       = "oauth2"
)

// rtEvent is the notification of one record change.
type rtEvent struct {
	Action     string         `json:"a"`
	Collection string         `json:"c"` // collection id
	Id         string         `json:"i"`
	Fields     map[string]any `json:"f,omitempty"` // delete only
}

// rtAuthEvent tells that the clients authenticated with a record of the
// collection must lose their auth (the token secret changed or the collection
// was deleted).
type rtAuthEvent struct {
	Collection string `json:"c"` // collection name
}

// bindRealtimeReplica publishes the local changes and receives the remote ones.
// It does nothing when the mode is off.
func bindRealtimeReplica(app core.App) {
	bus := core.ReplicaBusOf(app)
	if bus == nil {
		return
	}

	// the router can be built more than once per app (tests, restarts): bind once per bus
	boundKey := "__clvRealtimeReplica__" + bus.ID()
	if app.Store().Has(boundKey) {
		return
	}
	app.Store().Set(boundKey, true)

	publish := func(kind string, data any) { core.PublishReplicaEvent(app, kind, data) }

	// priority 100: after the local broadcast (-99) and the other fork hooks
	changed := func(action string) func(e *core.ModelEvent) error {
		return func(e *core.ModelEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			if record := realtimeResolveRecord(e.App, e.Model, ""); record != nil {
				publish(replicaEventRealtime, rtEvent{Action: action, Collection: record.Collection().Id, Id: record.Id})
			}

			return nil
		}
	}
	app.OnModelAfterCreateSuccess().Bind(&hook.Handler[*core.ModelEvent]{Id: "__clvRealtimeReplicaCreate__", Func: changed("create"), Priority: 100})
	app.OnModelAfterUpdateSuccess().Bind(&hook.Handler[*core.ModelEvent]{Id: "__clvRealtimeReplicaUpdate__", Func: changed("update"), Priority: 100})

	app.OnModelAfterDeleteSuccess().Bind(&hook.Handler[*core.ModelEvent]{
		Id: "__clvRealtimeReplicaDelete__",
		Func: func(e *core.ModelEvent) error {
			if err := e.Next(); err != nil {
				return err
			}

			var record *core.Record
			switch m := e.Model.(type) {
			case *core.Record:
				record = m
			case core.RecordProxy:
				record = m.ProxyRecord()
			}

			if record != nil {
				publish(replicaEventRealtime, rtEvent{
					Action:     "delete",
					Collection: record.Collection().Id,
					Id:         record.Id,
					Fields:     realtimeReplicaFields(record),
				})
			} else if collection := realtimeResolveRecordCollection(e.App, e.Model); collection != nil {
				// a custom model: only its identity is known
				if id, ok := e.Model.PK().(string); ok {
					publish(replicaEventRealtime, rtEvent{Action: "delete", Collection: collection.Id, Id: id})
				}
			}

			return nil
		},
		Priority: 100,
	})

	// the auth state of the clients of an auth collection whose token secret
	// changed (or that was deleted) is dropped on every replica
	app.OnCollectionUpdate().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: "__clvRealtimeReplicaAuthSecret__",
		Func: func(e *core.CollectionEvent) error {
			if !e.Collection.IsAuth() {
				return e.Next()
			}

			cached, _ := e.App.FindCachedCollectionByNameOrId(e.Collection.Id)

			if err := e.Next(); err != nil {
				return err
			}

			if cached != nil && cached.AuthToken.Secret != e.Collection.AuthToken.Secret {
				publish(replicaEventRealtimeAuth, rtAuthEvent{Collection: e.Collection.Name})
			}

			return nil
		},
		Priority: -98,
	})
	app.OnCollectionAfterDeleteSuccess().Bind(&hook.Handler[*core.CollectionEvent]{
		Id: "__clvRealtimeReplicaAuthDelete__",
		Func: func(e *core.CollectionEvent) error {
			if e.Collection.IsAuth() {
				publish(replicaEventRealtimeAuth, rtAuthEvent{Collection: e.Collection.Name})
			}
			return e.Next()
		},
		Priority: -98,
	})

	bus.On(replicaEventRealtime, func(m replica.Message) { handleRemoteRealtime(app, m) })
	bus.On(replicaEventRealtimeAuth, func(m replica.Message) { handleRemoteRealtimeAuth(app, m) })
	bus.On(replicaEventOAuth2, func(m replica.Message) { handleRemoteOAuth2(app, m) })
}

// realtimeReplicaFields returns the field values a remote replica needs to
// evaluate the rules of a deleted record. Password hashes and encrypted values
// stay out of the notification (a rule cannot use them anyway).
func realtimeReplicaFields(record *core.Record) map[string]any {
	collection := record.Collection()
	out := make(map[string]any, len(collection.Fields))

	for _, f := range collection.Fields {
		if f.Type() == core.FieldTypePassword || core.IsEncryptedFieldType(f.Type()) {
			continue
		}
		out[f.GetName()] = record.GetRaw(f.GetName())
	}

	return out
}

func hasLocalRealtimeClients(app core.App) bool {
	return len(app.SubscriptionsBroker().ChunkedClients(clientsChunkSize)) > 0
}

func handleRemoteRealtime(app core.App, m replica.Message) {
	if !hasLocalRealtimeClients(app) {
		return // nobody to tell: do not even read the record
	}

	var ev rtEvent
	if err := json.Unmarshal(m.Data, &ev); err != nil || ev.Id == "" {
		return
	}

	collection, err := app.FindCachedCollectionByNameOrId(ev.Collection)
	if err != nil {
		// created on another replica a moment ago: the cache has not caught up yet
		collection, err = app.FindCollectionByNameOrId(ev.Collection)
	}
	if err != nil || collection == nil {
		return
	}

	logFail := func(what string, err error) {
		app.Logger().Debug(what, slog.String("id", ev.Id), slog.String("collectionName", collection.Name), slog.String("error", err.Error()))
	}

	switch ev.Action {
	case "create", "update":
		record, err := app.FindRecordById(collection, ev.Id)
		if err != nil {
			return // already deleted again
		}

		if ev.Action == "update" && collection.IsAuth() {
			if err := realtimeUpdateClientsAuth(app, record); err != nil {
				logFail("Failed to update client(s) associated to the remotely updated auth record", err)
			}
		}

		if err := realtimeBroadcastRecord(app, ev.Action, record, false); err != nil {
			logFail("Failed to broadcast a remote record "+ev.Action, err)
		}
	case "delete":
		record := core.NewRecord(collection)
		record.Id = ev.Id
		for name, value := range ev.Fields {
			if name != core.FieldNameId {
				record.Set(name, value)
			}
		}

		if collection.IsAuth() {
			if err := realtimeUnsetClientsAuthByRecordModelOrProxy(app, record); err != nil {
				logFail("Failed to remove client(s) associated to the remotely deleted auth record", err)
			}
		}

		if err := realtimeBroadcastRemoteDelete(app, record); err != nil {
			logFail("Failed to broadcast a remote record delete", err)
		}
	}
}

var errRealtimeRollback = errors.New("rollback")

// realtimeBroadcastRemoteDelete evaluates the rules of a record that was
// deleted on another replica: it puts the row back inside a transaction, runs
// the regular broadcast against it and rolls the transaction back.
func realtimeBroadcastRemoteDelete(app core.App, record *core.Record) error {
	err := app.RunInTransaction(func(txApp core.App) error {
		if err := txApp.UnsafeWithoutHooks().SaveNoValidate(record); err != nil {
			return err
		}

		if err := realtimeBroadcastRecord(app, "delete", record, false, txApp); err != nil {
			return err
		}

		return errRealtimeRollback
	})

	if errors.Is(err, errRealtimeRollback) {
		return nil
	}

	return err
}

func handleRemoteRealtimeAuth(app core.App, m replica.Message) {
	var ev rtAuthEvent
	if err := json.Unmarshal(m.Data, &ev); err != nil || ev.Collection == "" {
		return
	}

	_ = realtimeUnsetClientsAuthByCollection(app, &core.Collection{Name: ev.Collection})
}

// ---------------------------------------------------------------------------
// OAuth2 redirect
// ---------------------------------------------------------------------------

// The OAuth2 popup flow reports its result through the realtime connection of
// the client that started it (the "state" is that connection's client id). With
// several replicas the provider's redirect can reach another replica than the
// one that holds the connection, so the redirect is forwarded over the bus and
// the replica that has the client delivers it.

type oauth2ForwardEvent struct {
	State string `json:"s"`
	Data  []byte `json:"d"`  // the JSON message for the client
	IP    string `json:"ip"` // the IP of the redirect request, for the XSRF check
	Code  string `json:"c"`
	Apple string `json:"a,omitempty"`
}

// plausibleClientId is a cheap filter so that a junk state does not become
// a notification.
func plausibleClientId(state string) bool {
	return len(state) >= 20 && len(state) <= 100 && !strings.ContainsAny(state, " \t\r\n")
}

// forwardOAuth2Redirect hands the redirect to the other replicas. It reports
// whether it did; the caller then answers like it does for a local client.
func forwardOAuth2Redirect(app core.App, data oauth2RedirectData, encoded []byte, ip string) bool {
	if core.ReplicaBusOf(app) == nil || !plausibleClientId(data.State) {
		return false
	}

	core.PublishReplicaEvent(app, replicaEventOAuth2, oauth2ForwardEvent{
		State: data.State,
		Data:  encoded,
		IP:    ip,
		Code:  data.Code,
		Apple: data.AppleUser,
	})

	return true
}

func handleRemoteOAuth2(app core.App, m replica.Message) {
	var ev oauth2ForwardEvent
	if err := json.Unmarshal(m.Data, &ev); err != nil {
		return
	}

	client, err := app.SubscriptionsBroker().ClientById(ev.State)
	if err != nil || client.IsDiscarded() || !client.HasSubscription(oauth2SubscriptionTopic) {
		return // the connection is on yet another replica
	}
	defer client.Unsubscribe(oauth2SubscriptionTopic)

	clientIP, _ := client.Get(RealtimeClientIPKey).(string)
	if clientIP != "" && clientIP != ev.IP {
		app.Logger().Debug("The client IP that completed the authentication is different from the one that initialized the OAuth2 realtime connection")
		return
	}

	if ev.Apple != "" && ev.Code != "" {
		_ = parseAndStoreAppleRedirectName(app, oauth2RedirectAppleNameStoreKeyPrefix+ev.Code, ev.Apple)
	}

	client.Send(subscriptions.Message{Name: oauth2SubscriptionTopic, Data: ev.Data})
}
