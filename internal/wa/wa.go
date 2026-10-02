// Package wa wraps whatsmeow: it pairs the linked device (QR shown in the web
// UI), forwards group messages and history syncs to the collector through a
// single ordered worker, resolves group names and backfills contact names.
// It replaces the old Node.js whatsapp-web.js bridge + socket.io collector.
package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"adesgo/internal/collector"
	"adesgo/internal/config"
	"adesgo/internal/db"
)

type Group struct {
	ID   string
	Name string
}

// Status is a snapshot for the web UI.
type Status struct {
	LoggedIn  bool
	Connected bool
	Pairing   bool
	QR        string // current QR payload while pairing
	Me        string
	PushName  string
	LastError string
	Queue     int
}

type Manager struct {
	cfg       *config.Config
	store     *db.Store
	collector *collector.Collector
	container *sqlstore.Container
	log       waLog.Logger

	mu        sync.RWMutex
	cli       *whatsmeow.Client
	qr        string
	pairing   bool
	lastError string

	queue chan []*collector.Message

	namesMu    sync.Mutex
	groupNames map[string]string
	namesAt    time.Time
}

func New(ctx context.Context, cfg *config.Config, st *db.Store, col *collector.Collector) (*Manager, error) {
	store.SetOSInfo("ADES Wine Club", [3]uint32{1, 0, 0})
	// Ask the phone for as much history as it's willing to share on pairing.
	store.DeviceProps.RequireFullSync = proto.Bool(true)

	// whatsmeow keeps its session tables (whatsmeow_*) in the same SQLite file,
	// on its own connection pool with foreign keys enabled as it requires.
	waDB, err := sql.Open("sqlite", db.DSN(cfg.DBPath, true))
	if err != nil {
		return nil, err
	}
	logger := waLog.Stdout("WA", "WARN", true)
	container := sqlstore.NewWithDB(waDB, "sqlite3", logger)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("whatsmeow store: %w", err)
	}
	m := &Manager{
		cfg: cfg, store: st, collector: col, container: container, log: logger,
		queue:      make(chan []*collector.Message, 256),
		groupNames: map[string]string{},
	}
	// The client always exists (Status, the /whatsapp page), even when Start
	// is never called (WA_DISABLED=1).
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	m.setClient(device)
	return m, nil
}

// Start connects (or starts pairing) and runs the background workers.
func (m *Manager) Start(ctx context.Context) error {
	go m.worker(ctx)
	go m.contactSyncLoop(ctx)
	return m.connect(ctx)
}

func (m *Manager) setClient(device *store.Device) {
	cli := whatsmeow.NewClient(device, m.log)
	cli.AddEventHandler(func(evt any) { m.onEvent(evt) })
	m.mu.Lock()
	m.cli = cli
	m.mu.Unlock()
}

func (m *Manager) client() *whatsmeow.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cli
}

func (m *Manager) connect(ctx context.Context) error {
	cli := m.client()
	if cli.Store.ID != nil {
		return cli.Connect()
	}
	return m.startPairing(ctx)
}

// startPairing requests QR codes; they are shown on /whatsapp and printed to
// the terminal. When all codes expire the client disconnects and the user can
// ask for a new QR from the web page.
func (m *Manager) startPairing(ctx context.Context) error {
	cli := m.client()
	qrChan, err := cli.GetQRChannel(context.WithoutCancel(ctx))
	if err != nil {
		return err
	}
	if err := cli.Connect(); err != nil {
		return err
	}
	m.mu.Lock()
	m.pairing, m.lastError = true, ""
	m.mu.Unlock()
	go func() {
		for item := range qrChan {
			switch item.Event {
			case "code":
				m.mu.Lock()
				m.qr = item.Code
				m.mu.Unlock()
				log.Printf("[wa] new QR code — scan it from %s/whatsapp or here:", m.cfg.AppURL)
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			case "success":
				log.Printf("[wa] pairing successful")
			default:
				m.mu.Lock()
				if item.Error != nil {
					m.lastError = item.Error.Error()
				} else if item.Event == "timeout" {
					m.lastError = "QR scaduto: generane uno nuovo."
				}
				m.mu.Unlock()
				log.Printf("[wa] pairing event: %s %v", item.Event, item.Error)
			}
		}
		m.mu.Lock()
		m.pairing, m.qr = false, ""
		m.mu.Unlock()
	}()
	return nil
}

// RestartPairing is used by the web UI after a QR timeout.
func (m *Manager) RestartPairing(ctx context.Context) error {
	cli := m.client()
	if cli.Store.ID != nil {
		return errors.New("dispositivo già collegato")
	}
	cli.Disconnect()
	return m.startPairing(ctx)
}

// Logout unlinks the device and immediately starts a new pairing.
func (m *Manager) Logout(ctx context.Context) error {
	cli := m.client()
	if cli.Store.ID != nil {
		if err := cli.Logout(ctx); err != nil {
			log.Printf("[wa] logout: %v", err)
			_ = cli.Store.Delete(ctx)
		}
	}
	cli.Disconnect()
	m.setClient(m.container.NewDevice())
	return m.startPairing(ctx)
}

func (m *Manager) Status() Status {
	cli := m.client()
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := Status{
		LoggedIn: cli.IsLoggedIn(), Connected: cli.IsConnected(),
		Pairing: m.pairing, QR: m.qr, LastError: m.lastError, Queue: len(m.queue),
	}
	if cli.Store.ID != nil {
		s.Me = cli.Store.ID.ToNonAD().String()
		s.PushName = cli.Store.PushName
	}
	return s
}

// ── events ───────────────────────────────────────────────────────────────────

func (m *Manager) onEvent(evt any) {
	switch e := evt.(type) {
	case *events.Message:
		if msg := m.convert(e, "live"); msg != nil {
			m.enqueue([]*collector.Message{msg})
		}
	case *events.UndecryptableMessage:
		// whatsmeow already asked the sender's phone to resend it; if that
		// fails too the message is lost, and only this line says so
		if m.cfg.IsChannel(e.Info.Chat.String()) {
			log.Printf("[wa] undecryptable message in %s from %s (%s) at %s (unavailable=%t): add it by hand if it was an order",
				e.Info.Chat, e.Info.Sender, e.Info.PushName, e.Info.Timestamp.Format("2006-01-02 15:04:05"), e.IsUnavailable)
		}
	case *events.HistorySync:
		go m.handleHistorySync(e)
	case *events.Connected:
		log.Printf("[wa] connected")
		go m.syncGroupPhones(context.Background())
		m.mu.Lock()
		m.lastError = ""
		m.mu.Unlock()
	case *events.Disconnected:
		log.Printf("[wa] disconnected (whatsmeow reconnects automatically)")
	case *events.LoggedOut:
		log.Printf("[wa] logged out by the phone (reason %v) — a new QR is needed", e.Reason)
		go func() {
			ctx := context.Background()
			m.client().Disconnect()
			m.setClient(m.container.NewDevice())
			if err := m.startPairing(ctx); err != nil {
				log.Printf("[wa] pairing: %v", err)
			}
		}()
	case *events.StreamReplaced:
		m.mu.Lock()
		m.lastError = "Sessione sostituita da un'altra istanza con le stesse credenziali."
		m.mu.Unlock()
	}
}

func (m *Manager) enqueue(batch []*collector.Message) {
	if len(batch) > 0 {
		m.queue <- batch
	}
}

// worker processes messages one at a time, in arrival order — the
// reply → listing linking depends on chronological processing.
func (m *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-m.queue:
			for _, msg := range batch {
				if err := m.collector.Handle(ctx, msg); err != nil {
					log.Printf("[collector] %s: %v", msg.ID, err)
				}
			}
		}
	}
}

func (m *Manager) handleHistorySync(e *events.HistorySync) {
	cli := m.client()
	var batch []*collector.Message
	for _, conv := range e.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil || !m.cfg.IsChannel(chatJID.String()) {
			continue
		}
		if name := conv.GetName(); name != "" {
			m.rememberGroupName(chatJID.String(), name)
		}
		for _, hm := range conv.GetMessages() {
			evt, err := cli.ParseWebMessage(chatJID, hm.GetMessage())
			if err != nil {
				continue
			}
			if msg := m.convert(evt, "history"); msg != nil {
				batch = append(batch, msg)
			}
		}
	}
	if len(batch) == 0 {
		return
	}
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Timestamp < batch[j].Timestamp })
	log.Printf("[wa] history sync (%s): %d messages from monitored groups", e.Data.GetSyncType(), len(batch))
	m.enqueue(batch)
}

// syncGroupPhones reads the members of the monitored groups and stores each
// member's phone number on their customer, when it is still empty (members
// who never wrote have no customer yet and are skipped).
func (m *Manager) syncGroupPhones(ctx context.Context) {
	cli := m.client()
	for _, id := range m.cfg.ChannelIDs {
		jid, err := types.ParseJID(id)
		if err != nil {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		gi, err := cli.GetGroupInfo(lctx, jid)
		cancel()
		if err != nil {
			log.Printf("[wa] group %s members: %v", id, err)
			continue
		}
		for _, p := range gi.Participants {
			lid, pn := p.LID, p.PhoneNumber
			if lid.IsEmpty() && p.JID.Server == types.HiddenUserServer {
				lid = p.JID
			}
			if pn.IsEmpty() && p.JID.Server == types.DefaultUserServer {
				pn = p.JID
			}
			if pn.IsEmpty() {
				continue
			}
			for _, user := range []types.JID{lid, pn} {
				if user.IsEmpty() {
					continue
				}
				if err := m.store.SetPhoneIfEmpty(ctx, legacyID(user), pn.User); err != nil {
					log.Printf("[wa] phone of %s: %v", legacyID(user), err)
				}
			}
		}
	}
	if n, err := m.store.FillPhones(ctx); err == nil && n > 0 {
		log.Printf("[wa] %d phone numbers filled from WhatsApp", n)
	}
}

// RequestHistory asks the phone for up to count messages older than the
// oldest one we have stored for chatID (arrives later as a HistorySync).
func (m *Manager) RequestHistory(ctx context.Context, chatID string, count int) error {
	cli := m.client()
	if !cli.IsLoggedIn() {
		return errors.New("WhatsApp non collegato")
	}
	serialized, ts, ok := m.store.OldestMessageID(ctx, chatID)
	if !ok {
		return errors.New("nessun messaggio con id WhatsApp in archivio per questa chat")
	}
	fromMe, chat, id, sender, ok := parseSerializedID(serialized)
	if !ok {
		return errors.New("id messaggio non valido: " + serialized)
	}
	chatJID, err := types.ParseJID(chat)
	if err != nil {
		return err
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chatJID, IsFromMe: fromMe, IsGroup: chatJID.Server == types.GroupServer},
		ID:            id,
		Timestamp:     time.Unix(ts, 0),
	}
	if sj, err := types.ParseJID(toWhatsmeowJID(sender)); err == nil {
		info.Sender = sj
	}
	_, err = cli.SendPeerMessage(ctx, cli.BuildHistorySyncRequest(info, count))
	return err
}

// ── conversion ───────────────────────────────────────────────────────────────

// convert normalizes a whatsmeow message to the collector format, producing
// the same ids the old whatsapp-web.js bridge stored (so existing rows,
// quoted-message lookups and dedup keep working).
func (m *Manager) convert(e *events.Message, source string) *collector.Message {
	info := e.Info
	if !info.IsGroup || !m.cfg.IsChannel(info.Chat.String()) {
		return nil
	}
	if e.IsEdit {
		log.Printf("[SKIP]    edited message ignored (%s, %s): %q", info.PushName, info.Timestamp.Format("2006-01-02 15:04:05"), extractEdit(e.Message))
		return nil
	}
	if e.Message == nil || e.Message.GetProtocolMessage() != nil || e.Message.GetReactionMessage() != nil {
		return nil
	}
	body, ctxInfo := extractText(e.Message)
	if strings.TrimSpace(body) == "" {
		return nil
	}
	ctx := context.Background()
	author, alt := m.preferLID(ctx, info.Sender, info.SenderAlt, info.IsFromMe)
	chat := info.Chat.String()

	msg := &collector.Message{
		ID:        fmt.Sprintf("%t_%s_%s_%s", info.IsFromMe, chat, info.ID, author),
		ChatID:    chat,
		ChatName:  m.GroupName(ctx, chat),
		AuthorID:  author,
		AuthorAlt: alt,
		Author:    m.displayName(ctx, info),
		FromMe:    info.IsFromMe,
		Body:      body,
		Timestamp: info.Timestamp.Unix(),
		Source:    source,
	}
	if ctxInfo != nil && ctxInfo.GetStanzaID() != "" {
		msg.QuotedID = ctxInfo.GetStanzaID()
	}
	return msg
}

// extractEdit returns the new text of an edit, for the log.
func extractEdit(msg *waE2E.Message) string {
	if pm := msg.GetProtocolMessage(); pm != nil && pm.GetEditedMessage() != nil {
		body, _ := extractText(pm.GetEditedMessage())
		return body
	}
	body, _ := extractText(msg)
	return body
}

func extractText(msg *waE2E.Message) (string, *waE2E.ContextInfo) {
	switch {
	case msg.GetConversation() != "":
		return msg.GetConversation(), nil
	case msg.GetExtendedTextMessage() != nil:
		t := msg.GetExtendedTextMessage()
		return t.GetText(), t.GetContextInfo()
	case msg.GetImageMessage() != nil:
		t := msg.GetImageMessage()
		return t.GetCaption(), t.GetContextInfo()
	case msg.GetVideoMessage() != nil:
		t := msg.GetVideoMessage()
		return t.GetCaption(), t.GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		t := msg.GetDocumentMessage()
		return t.GetCaption(), t.GetContextInfo()
	}
	return "", nil
}

// legacyID formats a JID the way whatsapp-web.js did: users as "<n>@c.us",
// hidden users as "<n>@lid", without device suffixes.
func legacyID(j types.JID) string {
	if j.IsEmpty() {
		return ""
	}
	j = j.ToNonAD()
	if j.Server == types.DefaultUserServer {
		return j.User + "@c.us"
	}
	return j.String()
}

func toWhatsmeowJID(id string) string {
	if strings.HasSuffix(id, "@c.us") {
		return strings.TrimSuffix(id, "@c.us") + "@" + types.DefaultUserServer
	}
	return id
}

// preferLID returns (author, alternative) ids, preferring the LID like the old
// bridge did in LID-addressed groups.
func (m *Manager) preferLID(ctx context.Context, sender, alt types.JID, fromMe bool) (string, string) {
	switch {
	case sender.Server == types.HiddenUserServer:
		return legacyID(sender), legacyID(alt)
	case alt.Server == types.HiddenUserServer:
		return legacyID(alt), legacyID(sender)
	}
	cli := m.client()
	if fromMe && !cli.Store.LID.IsEmpty() {
		return legacyID(cli.Store.LID), legacyID(sender)
	}
	if lid, err := cli.Store.LIDs.GetLIDForPN(ctx, sender.ToNonAD()); err == nil && !lid.IsEmpty() {
		return legacyID(lid), legacyID(sender)
	}
	return legacyID(sender), legacyID(alt)
}

func (m *Manager) displayName(ctx context.Context, info types.MessageInfo) string {
	if info.PushName != "" && info.PushName != "-" {
		return info.PushName
	}
	if info.IsFromMe && m.client().Store.PushName != "" {
		return m.client().Store.PushName
	}
	return m.contactName(ctx, info.Sender, info.SenderAlt)
}

func (m *Manager) contactName(ctx context.Context, jids ...types.JID) string {
	cli := m.client()
	for _, j := range jids {
		if j.IsEmpty() {
			continue
		}
		c, err := cli.Store.Contacts.GetContact(ctx, j.ToNonAD())
		if err != nil || !c.Found {
			continue
		}
		for _, n := range []string{c.FullName, c.FirstName, c.PushName, c.BusinessName} {
			if n = strings.TrimSpace(n); n != "" && !strings.Contains(n, "@") {
				return n
			}
		}
	}
	return ""
}

// parseSerializedID splits "false_<chat>_<id>_<sender>".
func parseSerializedID(s string) (fromMe bool, chat, id, sender string, ok bool) {
	parts := strings.SplitN(s, "_", 4)
	if len(parts) < 3 || (parts[0] != "true" && parts[0] != "false") {
		return false, "", "", "", false
	}
	if len(parts) == 4 {
		sender = parts[3]
	}
	return parts[0] == "true", parts[1], parts[2], sender, true
}

// ── groups ───────────────────────────────────────────────────────────────────

func (m *Manager) rememberGroupName(id, name string) {
	m.namesMu.Lock()
	m.groupNames[id] = name
	m.namesMu.Unlock()
}

// GroupName returns the cached subject of a group ("" if not known yet). It
// is called for every collected message, so it never hits the network or DB.
func (m *Manager) GroupName(_ context.Context, id string) string {
	m.namesMu.Lock()
	defer m.namesMu.Unlock()
	return m.groupNames[id]
}

// GroupNames returns {chat_id: name} for every configured channel: WhatsApp
// lookup (refreshed every minute), then the name seen in stored listings,
// then the raw id.
func (m *Manager) GroupNames(ctx context.Context) map[string]string {
	m.namesMu.Lock()
	stale := time.Since(m.namesAt) > time.Minute
	if stale {
		m.namesAt = time.Now()
	}
	m.namesMu.Unlock()

	cli := m.client()
	if stale && cli.IsLoggedIn() {
		for _, id := range m.cfg.ChannelIDs {
			jid, err := types.ParseJID(id)
			if err != nil {
				continue
			}
			lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			gi, err := cli.GetGroupInfo(lctx, jid)
			cancel()
			if err == nil && gi.Name != "" {
				m.rememberGroupName(id, gi.Name)
			}
		}
	}
	known := m.store.KnownChatNames(ctx)
	m.namesMu.Lock()
	defer m.namesMu.Unlock()
	out := map[string]string{}
	for _, id := range m.cfg.ChannelIDs {
		switch {
		case m.groupNames[id] != "":
			out[id] = m.groupNames[id]
		case known[id] != "":
			out[id] = known[id]
		default:
			out[id] = id
		}
	}
	return out
}

// JoinedGroups lists every group the account is in (to find CHANNEL_ID).
func (m *Manager) JoinedGroups(ctx context.Context) ([]Group, error) {
	cli := m.client()
	if !cli.IsLoggedIn() {
		return nil, errors.New("WhatsApp non collegato")
	}
	groups, err := cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, Group{ID: g.JID.String(), Name: g.Name})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// ── contacts ─────────────────────────────────────────────────────────────────

// contactSyncLoop backfills names of @lid users (port of sync_contacts.py),
// every 5 minutes.
func (m *Manager) contactSyncLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := m.SyncContacts(ctx); err != nil {
				log.Printf("[wa] contact sync: %v", err)
			} else if n > 0 {
				log.Printf("[wa] contact sync: %d users updated", n)
			}
		}
	}
}

func (m *Manager) SyncContacts(ctx context.Context) (int, error) {
	cli := m.client()
	if !cli.IsLoggedIn() {
		return 0, nil
	}
	ids, err := m.store.UnnamedLIDUsers(ctx)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, id := range ids {
		lid, err := types.ParseJID(id)
		if err != nil {
			continue
		}
		jids := []types.JID{lid}
		if pn, err := cli.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !pn.IsEmpty() {
			jids = append(jids, pn)
		}
		if name := m.contactName(ctx, jids...); name != "" {
			if ok, err := m.store.SetUserName(ctx, id, name); err == nil && ok {
				updated++
			}
		}
	}
	return updated, nil
}

// Close disconnects the client.
func (m *Manager) Close() {
	if cli := m.client(); cli != nil {
		cli.Disconnect()
	}
	_ = m.container.Close()
}
