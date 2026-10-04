package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	tokenEnv     = "RESTARTCHROMIUM_BOTTOKEN"
	authCodeFile = "authcode.txt"
	usersDir     = "users"
	cooldown     = time.Minute
	settleDelay  = 10 * time.Second
	callbackData = "restart"
)

type bot struct {
	token  string
	client *http.Client

	mu       sync.Mutex
	lastRun  time.Time
	authCode string
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

type tgMessage struct {
	Chat tgChat  `json:"chat"`
	From *tgUser `json:"from"`
	Text string  `json:"text"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	From    tgUser     `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID      int64       `json:"update_id"`
	Message       *tgMessage  `json:"message"`
	CallbackQuery *tgCallback `json:"callback_query"`
}

func (b *bot) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+url.PathEscape(b.token)+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		// Do not leak the token (part of the URL) via the error.
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("telegram %s: %w", method, ue.Err)
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()
	var res struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	if !res.OK {
		return fmt.Errorf("telegram %s: %s", method, res.Description)
	}
	if out != nil {
		return json.Unmarshal(res.Result, out)
	}
	return nil
}

func (u tgUser) displayName() string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if u.Username != "" {
		n += " (@" + u.Username + ")"
	}
	n = strings.Join(strings.Fields(n), " ")
	if n == "" {
		n = "unknown"
	}
	return n
}

func generateCode() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func userFile(chatID int64) string {
	return filepath.Join(usersDir, strconv.FormatInt(chatID, 10)+".txt")
}

func isAuthed(chatID int64) bool {
	_, err := os.Stat(userFile(chatID))
	return err == nil
}

func storeUser(chatID int64, u tgUser) error {
	if err := os.MkdirAll(usersDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(userFile(chatID), []byte(u.displayName()+"\n"), 0o600)
}

func (b *bot) sendButton(ctx context.Context, chatID int64, text string) {
	err := b.call(ctx, "sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    text,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{{
				{"text": "Restart Chromium", "callback_data": callbackData},
			}},
		},
	}, nil)
	if err != nil {
		log.Println(err)
	}
}

func (b *bot) sendText(ctx context.Context, chatID int64, text string) {
	if err := b.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text}, nil); err != nil {
		log.Println(err)
	}
}

func (b *bot) handleMessage(ctx context.Context, m *tgMessage) {
	chatID := m.Chat.ID
	if isAuthed(chatID) {
		b.sendButton(ctx, chatID, "Click the button to restart Chromium.")
		return
	}
	given := strings.ToLower(strings.Join(strings.Fields(m.Text), ""))
	if subtle.ConstantTimeCompare([]byte(given), []byte(b.authCode)) != 1 {
		b.sendText(ctx, chatID, "Please send the auth code.")
		return
	}
	var from tgUser
	if m.From != nil {
		from = *m.From
	}
	if err := storeUser(chatID, from); err != nil {
		log.Println("storing user:", err)
		b.sendText(ctx, chatID, "Internal error.")
		return
	}
	log.Printf("authorized chat %d (%s)", chatID, from.displayName())
	b.sendButton(ctx, chatID, "Authorized. Click the button to restart Chromium.")
}

func (b *bot) handleCallback(ctx context.Context, wg *sync.WaitGroup, c *tgCallback) {
	answer := func(text string) {
		if err := b.call(ctx, "answerCallbackQuery", map[string]any{
			"callback_query_id": c.ID, "text": text,
		}, nil); err != nil {
			log.Println(err)
		}
	}
	if c.Message == nil || c.Data != callbackData || !isAuthed(c.Message.Chat.ID) {
		answer("Not authorized.")
		return
	}
	b.mu.Lock()
	if wait := cooldown - time.Since(b.lastRun); !b.lastRun.IsZero() && wait > 0 {
		b.mu.Unlock()
		answer(fmt.Sprintf("Cooldown: try again in %d s.", int(wait.Seconds())+1))
		return
	}
	b.lastRun = time.Now()
	b.mu.Unlock()

	answer("Restarting Chromium...")
	chatID := c.Message.Chat.ID
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := restartChromium(ctx); err != nil {
			log.Println("restart:", err)
			b.sendText(context.WithoutCancel(ctx), chatID, "Restart failed: "+err.Error())
			return
		}
		b.sendText(context.WithoutCancel(ctx), chatID, "Chromium restarted.")
	}()
}

func readStat(pid int) (ppid int, state byte, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, 0, false
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 2 {
		return 0, 0, false
	}
	p, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, 0, false
	}
	return p, f[0][0], true
}

func listPIDs() []int {
	ents, _ := os.ReadDir("/proc")
	var pids []int
	for _, e := range ents {
		if p, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, p)
		}
	}
	return pids
}

// findChromium returns PIDs of .../chromium processes of the current user with PPID 1 and children.
func findChromium() []int {
	uid := uint32(os.Getuid())
	children := map[int]int{}
	ppids := map[int]int{}
	for _, pid := range listPIDs() {
		if pp, _, ok := readStat(pid); ok {
			ppids[pid] = pp
			children[pp]++
		}
	}
	var res []int
	for pid, pp := range ppids {
		if pp != 1 || children[pid] == 0 {
			continue
		}
		dir := "/proc/" + strconv.Itoa(pid)
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Uid != uid {
			continue
		}
		exe, err := os.Readlink(dir + "/exe")
		if err != nil {
			continue
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
		if filepath.Base(exe) != "chromium" {
			continue
		}
		res = append(res, pid)
	}
	return res
}

func alive(pid int) bool {
	_, state, ok := readStat(pid)
	return ok && state != 'Z' && state != 'X'
}

func restartChromium(ctx context.Context) error {
	pids := findChromium()
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			log.Printf("TERM %d: %v", pid, err)
		}
	}
	for _, pid := range pids {
		for alive(pid) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	if err := sleep(ctx, settleDelay); err != nil {
		return err
	}
	cmd := exec.Command("chromium")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap the child when it exits.
	go func() { _ = cmd.Wait() }()
	return sleep(ctx, settleDelay)
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func notify(state string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.Dial("unixgram", addr)
	if err != nil {
		log.Println("notify:", err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		log.Println("notify:", err)
	}
}

func run() error {
	token := os.Getenv(tokenEnv)
	if token == "" {
		return fmt.Errorf("%s is not set", tokenEnv)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	b := &bot{token: token, client: &http.Client{Timeout: 70 * time.Second}}
	var me tgUser
	if err := b.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return fmt.Errorf("bot token check failed: %w", err)
	}

	code, err := generateCode()
	if err != nil {
		return err
	}
	if err := os.WriteFile(authCodeFile, []byte(code+"\n"), 0o600); err != nil {
		return err
	}
	b.authCode = code

	notify("READY=1")
	log.Printf("started as @%s", me.Username)

	var wg sync.WaitGroup
	var offset int64
	for ctx.Err() == nil {
		var updates []tgUpdate
		err := b.call(ctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         50,
			"allowed_updates": []string{"message", "callback_query"},
		}, &updates)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Println(err)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			switch {
			case u.CallbackQuery != nil:
				b.handleCallback(ctx, &wg, u.CallbackQuery)
			case u.Message != nil:
				b.handleMessage(ctx, u.Message)
			}
		}
	}

	notify("STOPPING=1")
	wg.Wait()
	return nil
}

func main() {
	log.SetFlags(0)
	if err := run(); err != nil {
		log.Println(err)
		os.Exit(1)
	}
}
