package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/robfig/cron/v3"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/extra/bundebug"
	"github.com/uptrace/bun/extra/bunslog"
)

const name = "nostr-webhook"

const version = "0.0.122"

var revision = "HEAD"

var (
	feedRelays = []FeedRelay{
		{Relay: "wss://yabu.me", Enabled: true},
		{Relay: "wss://relay-jp.nostr.wirednet.jp", Enabled: false},
	}

	postRelays = []PostRelay{
		{Relay: "wss://nostr-relay.nokotaro.com", Enabled: false},
		{Relay: "wss://relay-jp.nostr.wirednet.jp", Enabled: true},
		{Relay: "wss://yabu.me", Enabled: true},
		{Relay: "wss://nostr.holybea.com", Enabled: false},
		{Relay: "wss://relay.snort.social", Enabled: false},
		{Relay: "wss://relay.damus.io", Enabled: true},
		{Relay: "wss://nos.lol", Enabled: true},
		{Relay: "wss://nostr.h3z.jp", Enabled: false},
	}

	//go:embed static
	assets embed.FS
)

// FeedRelay is struct for feed relay
type FeedRelay struct {
	Relay   string `bun:"relay,pk,notnull" json:"relay"`
	Enabled bool   `bun:"enabled,default:false" json:"enabled"`
}

// PostRelay is struct for post relay
type PostRelay struct {
	Relay   string `bun:"relay,pk,notnull" json:"relay"`
	Enabled bool   `bun:"enabled,default:false" json:"enabled"`
}

// Hook is struct for webhook
type Hook struct {
	bun.BaseModel `bun:"table:hook,alias:e"`

	Name        string    `bun:"name,pk,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description"`
	Kinds       string    `bun:"kinds,notnull,default:'1'" json:"kinds"`
	Author      string    `bun:"author,notnull" json:"author"`
	Pattern     string    `bun:"pattern,notnull" json:"pattern"`
	MentionTo   string    `bun:"mention_to,notnull" json:"mention_to"`
	MentionFrom string    `bun:"mention_from,notnull" json:"mention_from"`
	Endpoint    string    `bun:"endpoint,notnull" json:"endpoint"`
	Secret      string    `bun:"secret,notnull,default:random_string(12)" json:"secret"`
	Enabled     bool      `bun:"enabled,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	nums []int
	re   *regexp.Regexp
}

// Watch is struct for webhook
type Watch struct {
	bun.BaseModel `bun:"table:watch,alias:w"`

	Name        string    `bun:"name,pk,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description"`
	Author      string    `bun:"author,notnull" json:"author"`
	Pattern     string    `bun:"pattern,notnull" json:"pattern"`
	Endpoint    string    `bun:"endpoint,notnull" json:"endpoint"`
	Secret      string    `bun:"secret,notnull,default:random_string(12)" json:"secret"`
	Enabled     bool      `bun:"enabled,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	re *regexp.Regexp
}

// Task is struct for cronjob
type Task struct {
	bun.BaseModel `bun:"table:task,alias:t"`

	Name        string    `bun:"name,pk,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description"`
	Author      string    `bun:"author,notnull" json:"author"`
	Spec        string    `bun:"spec,notnull" json:"spec"`
	Endpoint    string    `bun:"endpoint,notnull" json:"endpoint"`
	Secret      string    `bun:"secret,notnull,default:random_string(12)" json:"secret"`
	Enabled     bool      `bun:"enabled,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	id cron.EntryID
}

// Proxy is struct for proxy
type Proxy struct {
	bun.BaseModel `bun:"table:proxy,alias:p"`

	Name      string    `bun:"name,pk,notnull" json:"name"`
	Password  string    `bun:"password,notnull,default:random_string(12)" json:"password"`
	Author    string    `bun:"author,notnull" json:"author"`
	Enabled   bool      `bun:"enabled,default:true" json:"enabled"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// Info is struct for /info
type Info struct {
	Relay   []string `json:"relay"`
	Version string   `json:"version"`
}

var (
	relayMu sync.Mutex

	watchesMu sync.Mutex
	watches   = []Watch{}
	hooksMu   sync.Mutex
	hooks     = []Hook{}
	tasksMu   sync.Mutex
	tasks     = []Task{}
	proxiesMu sync.Mutex
	proxies   = []Proxy{}

	jobs *cron.Cron

	reNormalize = regexp.MustCompile(`\bnostr:\w+\b`)
	reKinds     = regexp.MustCompile(`^\s*\d+(?:\s*,\s*\d+)*\s*$`)
)

func feedRelayNames() []string {
	names := []string{}
	for _, v := range feedRelays {
		if v.Enabled {
			names = append(names, v.Relay)
		}
	}
	return names
}

func doHttpReqOnce(req *http.Request, name string) bool {
	client := new(http.Client)
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		log.Printf("%v: Not found: %v", name, resp.StatusCode)
		return false
	}
	if resp.StatusCode != 200 {
		log.Printf("%v: Invalid status code: %v", name, resp.StatusCode)
		return false
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("%v: %v", name, err)
		return false
	}

	var eev nostr.Event
	var eevs []nostr.Event

	err = json.NewDecoder(bytes.NewReader(b)).Decode(&eev)
	if err != nil {
		err = json.NewDecoder(bytes.NewReader(b)).Decode(&eevs)
		if err != nil {
			log.Printf("%v: %v", name, err)
			return false
		}
	} else {
		eevs = []nostr.Event{eev}
	}

	relayMu.Lock()
	defer relayMu.Unlock()

	var wg sync.WaitGroup
	for _, r := range postRelays {
		if !r.Enabled {
			continue
		}
		r := r

		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				relay, err := nostr.RelayConnect(context.Background(), r.Relay)
				if err != nil {
					log.Printf("%v: %v: %v", name, r, err)
					continue
				}
				for _, vv := range eevs {
					err = relay.Publish(context.Background(), vv)
					if err != nil {
						log.Printf("%v: %v: %v", name, r, err)
						continue
					}
				}
				relay.Close()
				break
			}
		}()
	}
	wg.Wait()
	return true
}

func doHttpReq(req *http.Request, name string) {
	for i := 0; i < 3; i++ {
		if doHttpReqOnce(req, name) {
			return
		}
	}
}

func doWatchEntries(ev *nostr.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		log.Println(err)
		return
	}
	watchesMu.Lock()
	defer watchesMu.Unlock()
	for _, entry := range watches {
		if !entry.Enabled {
			continue
		}
		content := ev.Content
		//content = strings.TrimSpace(reNormalize.ReplaceAllString(content, ""))
		content = strings.TrimSpace(content)
		if entry.re != nil && !entry.re.MatchString(content) {
			continue
		}
		log.Printf("%v: Matched Watch entry", entry.Name)
		req, err := http.NewRequest(http.MethodPost, entry.Endpoint, bytes.NewReader(b))
		if err != nil {
			log.Printf("%v: %v", entry.Name, err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+entry.Secret)
		req.Header.Set("Accept", "application/json")
		go doHttpReq(req, entry.Name)
	}
}

func doHookEntries(ev *nostr.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		log.Println(err)
		return
	}
	hooksMu.Lock()
	defer hooksMu.Unlock()
	for _, entry := range hooks {
		if !entry.Enabled {
			continue
		}
		found := false
		for _, k := range entry.nums {
			if k == ev.Kind {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		content := ev.Content
		//content = strings.TrimSpace(reNormalize.ReplaceAllString(content, ""))
		content = strings.TrimSpace(content)
		if entry.re != nil && !entry.re.MatchString(content) {
			continue
		}
		if entry.MentionTo != "" {
			found := false
			for _, tag := range ev.Tags {
				if tag.Key() != "p" {
					continue
				}
				if tag.Value() == entry.MentionTo {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		if entry.MentionFrom != "" {
			if pub, err := nip19.EncodePublicKey(ev.PubKey); err != nil {
				continue
			} else if pub != entry.MentionFrom {
				continue
			}
		}
		log.Printf("%v: Matched Hook entry", entry.Name)
		req, err := http.NewRequest(http.MethodPost, entry.Endpoint, bytes.NewReader(b))
		if err != nil {
			log.Printf("%v: %v", entry.Name, err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+entry.Secret)
		req.Header.Set("Accept", "application/json")
		go doHttpReq(req, entry.Name)
	}
}

func reloadTasks(bundb *bun.DB) {
	log.Printf("Reload tasks")

	var ee []Task
	err := bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Scan(context.Background(), &ee)
	if err != nil {
		log.Println(err)
		return
	}

	<-jobs.Stop().Done()
	for _, cr := range tasks {
		jobs.Remove(cr.id)
	}
	for i := range ee {
		ct := ee[i]
		if !ct.Enabled {
			continue
		}
		id, err := jobs.AddFunc(ct.Spec, func() {
			log.Printf("%v: Start", ct.Name)
			req, err := http.NewRequest(http.MethodGet, ct.Endpoint, nil)
			if err != nil {
				log.Printf("%v: %v", ct.Name, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+ct.Secret)
			req.Header.Set("Accept", "application/json")
			client := new(http.Client)
			client.Timeout = 30 * time.Second
			resp, err := client.Do(req)
			if err != nil {
				log.Println(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				log.Printf("%v: Invalid status code: %v", ct.Name, resp.StatusCode)
				return
			}
			var eev nostr.Event
			err = json.NewDecoder(resp.Body).Decode(&eev)
			if err != nil {
				log.Printf("%v: %v", ct.Name, err)
				return
			}
			relayMu.Lock()
			defer relayMu.Unlock()
			for _, r := range postRelays {
				if !r.Enabled {
					continue
				}
				for i := 0; i < 3; i++ {
					relay, err := nostr.RelayConnect(context.Background(), r.Relay)
					if err != nil {
						log.Printf("%v: %v: %v", ct.Name, r, err)
						continue
					}
					err = relay.Publish(context.Background(), eev)
					relay.Close()
					if err != nil {
						log.Printf("%v: %v: %v", ct.Name, r, err)
						continue
					}
					break
				}
			}
		})
		if err != nil {
			log.Printf("%v: Failed to add task: %v", ct.Name, err)
			continue
		}
		ee[i].id = id
	}
	jobs.Start()

	tasksMu.Lock()
	defer tasksMu.Unlock()
	tasks = ee
	log.Printf("Reloaded %d tasks", len(ee))
}

func reloadFeedRelays(bundb *bun.DB) {
	log.Printf("Reload feed relays")

	var ee []FeedRelay
	err := bundb.NewSelect().Model((*FeedRelay)(nil)).Scan(context.Background(), &ee)
	if err != nil {
		log.Println(err)
		return
	}

	relayMu.Lock()
	defer relayMu.Unlock()
	feedRelays = ee
	log.Printf("Reloaded %d feed relays", len(ee))
}

func reloadPostRelays(bundb *bun.DB) {
	log.Printf("Reload post relays")

	var ee []PostRelay
	err := bundb.NewSelect().Model((*PostRelay)(nil)).Scan(context.Background(), &ee)
	if err != nil {
		log.Println(err)
		return
	}

	relayMu.Lock()
	defer relayMu.Unlock()
	postRelays = ee
	log.Printf("Reloaded %d post relays", len(ee))
}

func reloadWatches(bundb *bun.DB) {
	log.Printf("Reload watches")

	var ee []Watch
	err := bundb.NewSelect().Model((*Watch)(nil)).Scan(context.Background(), &ee)
	if err != nil {
		log.Println(err)
		return
	}
	for i := range ee {
		if ee[i].Pattern != "" {
			ee[i].re, err = regexp.Compile(ee[i].Pattern)
			if err != nil {
				log.Fatal(err)
			}
		}
	}

	watchesMu.Lock()
	defer watchesMu.Unlock()
	watches = ee
	log.Printf("Reloaded %d watches", len(ee))
}

func reloadHooks(bundb *bun.DB) {
	log.Printf("Reload hooks")

	var ee []Hook
	err := bundb.NewSelect().Model((*Hook)(nil)).Scan(context.Background(), &ee)
	if err != nil {
		log.Println(err)
		return
	}
	for i := range ee {
		ee[i].nums = toNumbers(ee[i].Kinds)
		if ee[i].Pattern != "" {
			ee[i].re, err = regexp.Compile(ee[i].Pattern)
			if err != nil {
				log.Fatal(err)
			}
		}
		if ee[i].MentionTo != "" {
			_, npub, err := nip19.Decode(ee[i].MentionTo)
			if err != nil {
				log.Fatal(err)
			}
			ee[i].MentionTo = npub.(string)
		}
	}

	hooksMu.Lock()
	defer hooksMu.Unlock()
	hooks = ee
	log.Printf("Reloaded %d hooks", len(ee))
}

func reloadProxies(bundb *bun.DB) {
	log.Printf("Reload proxies")

	var ee []Proxy
	err := bundb.NewSelect().Model((*Proxy)(nil)).Scan(context.Background(), &ee)
	if err != nil {
		log.Println(err)
		return
	}

	proxiesMu.Lock()
	defer proxiesMu.Unlock()
	proxies = ee
	log.Printf("Reloaded %d proxies", len(ee))
}

func heartbeatPush(url string) {
	resp, err := http.Get(url)
	if err != nil {
		log.Println(err.Error())
		return
	}
	defer resp.Body.Close()
}

func server(from *time.Time) {
	enc := json.NewEncoder(os.Stdout)

	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Println(err)
		return
	}

	bundb := bun.NewDB(db, pgdialect.New())
	defer bundb.Close()

	_, err = bundb.NewCreateTable().Model((*FeedRelay)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadFeedRelays(bundb)

	_, err = bundb.NewCreateTable().Model((*PostRelay)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadPostRelays(bundb)

	_, err = bundb.NewCreateTable().Model((*Hook)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadHooks(bundb)

	_, err = bundb.NewCreateTable().Model((*Watch)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadWatches(bundb)

	_, err = bundb.NewCreateTable().Model((*Task)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadTasks(bundb)

	_, err = bundb.NewCreateTable().Model((*Proxy)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadProxies(bundb)

	log.Println("Connecting to relay")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := nostr.NewSimplePool(ctx)
	if err != nil {
		log.Println(err)
		return
	}

	log.Println("Connected to relay")

	events := make(chan *nostr.Event, 100)
	defer close(events)

	timestamp := nostr.Timestamp(from.Unix())
	filters := []nostr.Filter{{
		//Kinds: []int{nostr.KindTextNote, nostr.KindChannelMessage, nostr.KindProfileMetadata},
		Since: &timestamp,
	}}
	sub := pool.SubMany(ctx, feedRelayNames(), filters)
	defer close(sub)

	hbtimer := time.NewTicker(5 * time.Minute)
	defer hbtimer.Stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func(wg *sync.WaitGroup, events chan *nostr.Event) {
		defer cancel()
		defer wg.Done()

		retry := 0
		log.Println("Start")
	events_loop:
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					break events_loop
				}
				enc.Encode(ev)
				/*
					switch ev.Kind {
					case nostr.KindProfileMetadata:
						doWatchEntries(ev)
					case nostr.KindTextNote, nostr.KindChannelMessage:
						doHookEntries(ev)
					}
				*/
				doHookEntries(ev)
				if ev.CreatedAt.Time().After(*from) {
					*from = ev.CreatedAt.Time()
				}
				retry = 0
			case <-hbtimer.C:
				if url := os.Getenv("HEARTBEAT_URL"); url != "" {
					go heartbeatPush(url)
				}
			case <-time.After(10 * time.Second):
				alive := pool.Relays.Size()
				pool.Relays.Range(func(key string, relay *nostr.Relay) bool {
					if relay.ConnectionError != nil {
						log.Println(relay.ConnectionError, relay.IsConnected())
						alive--
					}
					return true
				})
				if alive == 0 {
					break events_loop
				}
				retry++
				log.Println("Health check", retry)
				if retry > 60 {
					break events_loop
				}
			}
		}
		log.Println("Finish")
	}(&wg, events)

	log.Println("Subscribing events")

loop:
	for {
		select {
		case ev, ok := <-sub:
			if !ok || ev.Event == nil {
				break loop
			}
			events <- ev.Event
		case <-pool.Context.Done():
			break loop
		}
	}
	wg.Wait()

	log.Println("Stopped")
}

func jwtName(c echo.Context) (string, error) {
	cookie, err := c.Request().Cookie("CF_Authorization")
	if err != nil {
		return "", nil
	}

	claims := jwt.MapClaims{}
	parser := new(jwt.Parser)
	_, _, err = parser.ParseUnverified(cookie.Value, &claims)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]interface{}(claims))
	log.Println(string(b))
	iss, ok := map[string]interface{}(claims)["iss"]
	if !ok {
		return "unknown", nil
	}
	req, err := http.NewRequest(http.MethodGet, iss.(string)+"/cdn-cgi/access/get-identity", nil)
	if err != nil {
		return "", err
	}
	req.Header.Add("Cookie", "CF_Authorization="+cookie.Value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	err = json.NewDecoder(resp.Body).Decode(&claims)
	if err != nil {
		return "", err
	}

	name, ok := map[string]interface{}(claims)["name"]
	if !ok {
		return "unknown", nil
	}

	return name.(string), nil
}

func jwtEmail(c echo.Context) (string, error) {
	cookie, err := c.Request().Cookie("CF_Authorization")
	if err != nil {
		return "", err
	}

	claims := jwt.MapClaims{}
	parser := new(jwt.Parser)
	_, _, err = parser.ParseUnverified(cookie.Value, &claims)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(map[string]interface{}(claims))
	log.Println(string(b))
	if email, ok := map[string]interface{}(claims)["email"]; ok {
		return email.(string), nil
	}
	return "unknown", nil
}

func checkWatch(c echo.Context, watch *Watch) (bool, error) {
	if err := c.Bind(&watch); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	watch.Name = strings.TrimSpace(watch.Name)
	watch.Description = strings.TrimSpace(watch.Description)
	watch.Author = strings.TrimSpace(watch.Author)
	watch.Pattern = strings.TrimSpace(watch.Pattern)
	watch.Endpoint = strings.TrimSpace(watch.Endpoint)
	watch.Secret = strings.TrimSpace(watch.Secret)

	if watch.Name == "" {
		return false, c.JSON(http.StatusBadRequest, "Name must not be empty")
	}
	if watch.Endpoint == "" {
		return false, c.JSON(http.StatusBadRequest, "Endpoint must not be empty")
	}
	if email, err := jwtEmail(c); err == nil {
		watch.Author = email
	} else {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	if watch.Pattern == "" {
		return false, c.JSON(http.StatusBadRequest, "Pattern is invalid regular expression")
	}
	if _, err := regexp.Compile(watch.Pattern); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusBadRequest, "Pattern is invalid regular expression")
	}
	if _, err := url.Parse(watch.Endpoint); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusBadRequest, "Endpoint is invalid URL")
	}
	return true, nil
}

func toNumbers(s string) []int {
	nums := []int{}
	for _, ss := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(ss); err == nil && n >= 0 {
			nums = append(nums, n)
		}
	}
	return nums
}

func trimNumbers(s string) string {
	nums := []string{}
	for _, ss := range strings.Split(s, ",") {
		nums = append(nums, strings.TrimSpace(ss))
	}
	return strings.Join(nums, ",")
}

func checkHook(c echo.Context, hook *Hook) (bool, error) {
	if err := c.Bind(&hook); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	hook.Name = strings.TrimSpace(hook.Name)
	hook.Description = strings.TrimSpace(hook.Description)
	hook.Kinds = trimNumbers(hook.Kinds)
	hook.Author = strings.TrimSpace(hook.Author)
	hook.Pattern = strings.TrimSpace(hook.Pattern)
	hook.MentionTo = strings.TrimSpace(hook.MentionTo)
	hook.Endpoint = strings.TrimSpace(hook.Endpoint)
	hook.Secret = strings.TrimSpace(hook.Secret)

	if hook.Name == "" {
		return false, c.JSON(http.StatusBadRequest, "Name must not be empty")
	}
	if !reKinds.MatchString(hook.Kinds) {
		return false, c.JSON(http.StatusBadRequest, "Kinds should be numbers separated by comma")
	}
	if hook.Endpoint == "" {
		return false, c.JSON(http.StatusBadRequest, "Endpoint must not be empty")
	}
	if email, err := jwtEmail(c); err == nil {
		hook.Author = email
	} else {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	if hook.Pattern != "" {
		if _, err := regexp.Compile(hook.Pattern); err != nil {
			log.Println(err)
			return false, c.JSON(http.StatusBadRequest, "Pattern is invalid regular expression")
		}
	}
	if _, err := url.Parse(hook.Endpoint); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusBadRequest, "Endpoint is invalid URL")
	}
	if hook.MentionTo != "" {
		if _, _, err := nip19.Decode(hook.MentionTo); err != nil {
			log.Println(err)
			return false, c.JSON(http.StatusBadRequest, "MentionTo is not npub format")
		}
	}
	return true, nil
}

func checkTask(c echo.Context, task *Task) (bool, error) {
	if err := c.Bind(&task); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	task.Name = strings.TrimSpace(task.Name)
	task.Description = strings.TrimSpace(task.Description)
	task.Author = strings.TrimSpace(task.Author)
	task.Endpoint = strings.TrimSpace(task.Endpoint)
	task.Secret = strings.TrimSpace(task.Secret)

	if task.Name == "" {
		return false, c.JSON(http.StatusBadRequest, "Name must not be empty")
	}
	if task.Endpoint == "" {
		return false, c.JSON(http.StatusBadRequest, "Endpoint must not be empty")
	}
	if email, err := jwtEmail(c); err == nil {
		task.Author = email
	} else {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	if _, err := cron.ParseStandard(task.Spec); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusBadRequest, "Spec is invalid crontab expression")
	}
	if _, err := url.Parse(task.Endpoint); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusBadRequest, "Endpoint is invalid URL")
	}
	return true, nil
}

func checkProxy(c echo.Context, proxy *Proxy) (bool, error) {
	if err := c.Bind(&proxy); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	if email, err := jwtEmail(c); err == nil {
		proxy.Author = email
	} else {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	if name, err := jwtName(c); err == nil {
		proxy.Name = name
	} else {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	return true, nil
}

func manager() {
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	bundb := bun.NewDB(db, pgdialect.New())
	bundb.AddQueryHook(
		bundebug.NewQueryHook(
			bundebug.WithVerbose(true),
			bundebug.FromEnv("BUNDEBUG"),
		),
	)
	bundb.AddQueryHook(
		bunslog.NewQueryHook(
			bunslog.WithQueryLogLevel(slog.LevelDebug),
			bunslog.WithSlowQueryLogLevel(slog.LevelWarn),
			bunslog.WithErrorQueryLogLevel(slog.LevelError),
			bunslog.WithSlowQueryThreshold(3*time.Second),
		),
	)
	defer bundb.Close()

	_, err = bundb.NewCreateTable().Model((*Hook)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}

	e := echo.New()
	e.Use(middleware.Logger())
	e.IPExtractor = echo.ExtractIPFromXFFHeader()

	sub, _ := fs.Sub(assets, "static")
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(sub))))

	e.GET("/watches", func(c echo.Context) error {
		var watches []Watch
		err := bundb.NewSelect().Model((*Watch)(nil)).Order("created_at").Scan(context.Background(), &watches)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, watches)
	})
	e.GET("/watches/:name", func(c echo.Context) error {
		var watch Watch
		err := bundb.NewSelect().Model((*Watch)(nil)).Where("name = ?", c.Param("name")).Scan(context.Background(), &watch)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, watch)
	})
	e.POST("/watches/", func(c echo.Context) error {
		var watch Watch
		if ok, err := checkWatch(c, &watch); !ok {
			return err
		}
		_, err := bundb.NewInsert().Model(&watch).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		reloadWatches(bundb)
		return c.JSON(http.StatusOK, watch)
	})
	e.POST("/watches/:name", func(c echo.Context) error {
		var watch Watch
		if ok, err := checkWatch(c, &watch); !ok {
			return err
		}
		result, err := bundb.NewUpdate().Model(&watch).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		if num, err := result.RowsAffected(); err != nil || num == 0 {
			return c.JSON(http.StatusInternalServerError, "No records updated")
		}
		reloadWatches(bundb)
		return c.JSON(http.StatusOK, watch)
	})
	e.DELETE("/watches/:name", func(c echo.Context) error {
		result, err := bundb.NewDelete().Model((*Watch)(nil)).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		if num, err := result.RowsAffected(); err != nil || num == 0 {
			return c.JSON(http.StatusInternalServerError, "No records deleted")
		}
		reloadWatches(bundb)
		return c.JSON(http.StatusOK, c.Param("name"))
	})

	e.GET("/hooks", func(c echo.Context) error {
		var hooks []Hook
		err := bundb.NewSelect().Model((*Hook)(nil)).Order("created_at").Scan(context.Background(), &hooks)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hooks)
	})
	e.GET("/hooks/:name", func(c echo.Context) error {
		var hook Hook
		err := bundb.NewSelect().Model((*Hook)(nil)).Where("name = ?", c.Param("name")).Scan(context.Background(), &hook)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.POST("/hooks/", func(c echo.Context) error {
		var hook Hook
		if ok, err := checkHook(c, &hook); !ok {
			return err
		}
		_, err := bundb.NewInsert().Model(&hook).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		reloadHooks(bundb)
		return c.JSON(http.StatusOK, hook)
	})
	e.POST("/hooks/:name", func(c echo.Context) error {
		var hook Hook
		if ok, err := checkHook(c, &hook); !ok {
			return err
		}
		result, err := bundb.NewUpdate().Model(&hook).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		if num, err := result.RowsAffected(); err != nil || num == 0 {
			return c.JSON(http.StatusInternalServerError, "No records updated")
		}
		reloadHooks(bundb)
		return c.JSON(http.StatusOK, hook)
	})
	e.DELETE("/hooks/:name", func(c echo.Context) error {
		result, err := bundb.NewDelete().Model((*Hook)(nil)).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		if num, err := result.RowsAffected(); err != nil || num == 0 {
			return c.JSON(http.StatusInternalServerError, "No records deleted")
		}
		reloadHooks(bundb)
		return c.JSON(http.StatusOK, c.Param("name"))
	})

	e.GET("/tasks", func(c echo.Context) error {
		var tasks []Task
		err := bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Scan(context.Background(), &tasks)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, tasks)
	})
	e.GET("/tasks/:name", func(c echo.Context) error {
		var task Task
		err := bundb.NewSelect().Model((*Task)(nil)).Where("name = ?", c.Param("name")).Scan(context.Background(), &task)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, task)
	})
	e.POST("/tasks/", func(c echo.Context) error {
		var task Task
		if ok, err := checkTask(c, &task); !ok {
			return err
		}
		_, err := bundb.NewInsert().Model(&task).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		reloadTasks(bundb)
		return c.JSON(http.StatusOK, task)
	})
	e.POST("/tasks/:name", func(c echo.Context) error {
		var task Task
		if ok, err := checkTask(c, &task); !ok {
			return err
		}
		result, err := bundb.NewUpdate().Model(&task).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		if num, err := result.RowsAffected(); err != nil || num == 0 {
			return c.JSON(http.StatusInternalServerError, "No records updated")
		}
		reloadTasks(bundb)
		return c.JSON(http.StatusOK, task)
	})
	e.DELETE("/tasks/:name", func(c echo.Context) error {
		result, err := bundb.NewDelete().Model((*Task)(nil)).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		if num, err := result.RowsAffected(); err != nil || num == 0 {
			return c.JSON(http.StatusInternalServerError, "No records deleted")
		}
		reloadTasks(bundb)
		return c.JSON(http.StatusOK, c.Param("name"))
	})

	e.GET("/proxy", func(c echo.Context) error {
		var proxy Proxy
		name, err := jwtName(c)
		if err != nil {
			log.Println(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		err = bundb.NewSelect().Model((*Proxy)(nil)).Where("name = ?", name).Scan(context.Background(), &proxy)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, proxy)
	})
	e.POST("/proxy", func(c echo.Context) error {
		var proxy Proxy
		if ok, err := checkProxy(c, &proxy); !ok {
			return err
		}
		bundb.NewDelete().Model((*Proxy)(nil)).Where("name = ?", proxy.Name).Exec(context.Background())
		_, err := bundb.NewInsert().Model(&proxy).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		reloadProxies(bundb)
		return c.JSON(http.StatusOK, proxy)
	})

	e.POST("/post", func(c echo.Context) error {
		name, password, ok := c.Request().BasicAuth()
		if !ok {
			return c.JSON(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		}

		proxiesMu.Lock()
		defer proxiesMu.Unlock()
		ok = false
		for _, p := range proxies {
			if p.Name == name && p.Password == password && p.Enabled {
				ok = true
				break
			}
		}
		if !ok {
			return c.JSON(http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		}

		var eev nostr.Event
		err := json.NewDecoder(c.Request().Body).Decode(&eev)
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
		}
		relayMu.Lock()
		defer relayMu.Unlock()

		var wg sync.WaitGroup
		for _, r := range postRelays {
			if !r.Enabled {
				continue
			}
			r := r

			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 3; i++ {
					relay, err := nostr.RelayConnect(context.Background(), r.Relay)
					if err != nil {
						log.Printf("%v: %v: %v", name, r, err)
						continue
					}
					err = relay.Publish(context.Background(), eev)
					relay.Close()
					if err != nil {
						log.Printf("%v: %v: %v", name, r, err)
						continue
					}
					break
				}
			}()
		}
		return c.JSON(http.StatusCreated, "")
	})

	e.GET("/reload", func(c echo.Context) error {
		reloadFeedRelays(bundb)
		reloadPostRelays(bundb)
		return c.JSON(http.StatusOK, "OK")
	})
	e.GET("/info", func(c echo.Context) error {
		relayMu.Lock()
		defer relayMu.Unlock()
		info := Info{
			Relay:   feedRelayNames(),
			Version: version,
		}
		return c.JSON(http.StatusOK, info)
	})

	e.Logger.Fatal(e.Start(":8989"))
}

func init() {
	time.Local = time.FixedZone("Local", 9*60*60)
	jobs = cron.New(cron.WithLocation(time.Local))
}

func main() {
	var ver bool
	flag.BoolVar(&ver, "version", false, "show version")
	flag.Parse()

	if ver {
		fmt.Println(version)
		os.Exit(0)
	}

	go http.ListenAndServe("0.0.0.0:6060", nil)

	go func() {
		from := time.Now()
		for {
			server(&from)
			time.Sleep(5 * time.Second)
		}
	}()

	manager()
}
