package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

const version = "0.0.91"

var revision = "HEAD"

var (
	feedRelays = []FeedRelay{
		{Relay: "wss://relay-jp.nostr.wirednet.jp", Enabled: true},
		{Relay: "wss://nostr-relay.nokotaro.com", Enabled: true},
	}
	feedIndex = 0

	postRelays = []PostRelay{
		{Relay: "wss://nostr-relay.nokotaro.com", Enabled: true},
		{Relay: "wss://relay-jp.nostr.wirednet.jp", Enabled: true},
		{Relay: "wss://nostr.holybea.com", Enabled: true},
		{Relay: "wss://relay.snort.social", Enabled: true},
		{Relay: "wss://relay.damus.io", Enabled: true},
		{Relay: "wss://relay.nostrich.land", Enabled: true},
		{Relay: "wss://nostr.h3z.jp", Enabled: true},
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
	Author      string    `bun:"author,notnull" json:"author"`
	Pattern     string    `bun:"pattern,notnull" json:"pattern"`
	MentionTo   string    `bun:"mention_to,notnull" json:"mention_to"`
	MentionFrom string    `bun:"mention_from,notnull" json:"mention_from"`
	Endpoint    string    `bun:"endpoint,notnull" json:"endpoint"`
	Secret      string    `bun:"secret,notnull,default:random_string(12)" json:"secret"`
	Enabled     bool      `bun:"enabled,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	re *regexp.Regexp
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
	Enabled     bool      `bun:"enabled,notnull,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	id cron.EntryID
}

// Proxy is struct for proxy
type Proxy struct {
	bun.BaseModel `bun:"table:proxy,alias:p"`

	Name      string    `bun:"name,pk,notnull" json:"name"`
	Password  string    `bun:"password,notnull,default:random_string(12)" json:"password"`
	Author    string    `bun:"author,notnull" json:"author"`
	Enabled   bool      `bun:"enabled,notnull,default:true" json:"enabled"`
	CreatedAt time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
}

// Info is struct for /info
type Info struct {
	Relay   string `json:"relay"`
	Version string `json:"version"`
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
)

func switchFeedRelay() {
	relayMu.Lock()
	defer relayMu.Unlock()

	for {
		feedIndex++
		if feedRelays[feedIndex%len(feedRelays)].Enabled {
			break
		}
	}
}

func doHttpReqOnce(req *http.Request, name string, ev *nostr.Event) bool {
	client := new(http.Client)
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		log.Println(err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("%v: Invalid status code: %v", name, resp.StatusCode)
		return false
	}
	var eev nostr.Event
	err = json.NewDecoder(resp.Body).Decode(&eev)
	if err != nil {
		log.Printf("%v: %v", name, err)
		return false
	}
	if (eev.Kind == nostr.KindTextNote || eev.Kind == nostr.KindChannelMessage) && (eev.Kind != ev.Kind && ev.Kind != nostr.KindProfileMetadata) {
		log.Printf("%v: Invalid kind for %v: %v", name, ev.Kind, eev.Kind)
		return false
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
				_, err = relay.Publish(context.Background(), eev)
				relay.Close()
				if err != nil {
					log.Printf("%v: %v: %v", name, r, err)
					continue
				}
				break
			}
		}()
	}
	wg.Wait()
	return true
}

func doHttpReq(req *http.Request, name string, ev *nostr.Event) {
	for i := 0; i < 3; i++ {
		if doHttpReqOnce(req, name, ev) {
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
		content = strings.TrimSpace(reNormalize.ReplaceAllString(content, ""))
		if !entry.re.MatchString(content) {
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
		go doHttpReq(req, entry.Name, ev)
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
		content := ev.Content
		content = strings.TrimSpace(reNormalize.ReplaceAllString(content, ""))
		if !entry.re.MatchString(content) {
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
		go doHttpReq(req, entry.Name, ev)
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
					_, err = relay.Publish(context.Background(), eev)
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
	relayMu.Lock()
	relay, err := nostr.RelayConnect(context.Background(), feedRelays[feedIndex%len(feedRelays)].Relay)
	relayMu.Unlock()
	if err != nil {
		switchFeedRelay()
		log.Println(err)
		return
	}
	defer relay.Close()

	log.Println("Connected to relay")

	events := make(chan *nostr.Event, 100)
	defer close(events)

	timestamp := nostr.Timestamp(from.Unix())
	filters := []nostr.Filter{{
		Kinds: []int{nostr.KindTextNote, nostr.KindChannelMessage, nostr.KindProfileMetadata},
		Since: &timestamp,
	}}
	sub, err := relay.Subscribe(context.Background(), filters)
	if err != nil {
		switchFeedRelay()
		log.Println(err)
		return
	}
	defer sub.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func(wg *sync.WaitGroup, events chan *nostr.Event) {
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
				switch ev.Kind {
				case nostr.KindProfileMetadata:
					doWatchEntries(ev)
				case nostr.KindTextNote, nostr.KindChannelMessage:
					doHookEntries(ev)
				}
				if ev.CreatedAt.Time().After(*from) {
					*from = ev.CreatedAt.Time()
				}
				retry = 0
			case <-time.After(10 * time.Second):
				if relay.ConnectionError != nil {
					switchFeedRelay()
					log.Println(relay.ConnectionError)
					break events_loop
				}
				retry++
				log.Println("Health check", retry)
				if retry > 60 {
					switchFeedRelay()
					break events_loop
				}
			}
		}
		for range events {
		}
		log.Println("Finish")
	}(&wg, events)

	log.Println("Subscribing events")

loop:
	for {
		ev, ok := <-sub.Events
		if !ok || ev == nil {
			break loop
		}
		events <- ev
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

func checkHook(c echo.Context, hook *Hook) (bool, error) {
	if err := c.Bind(&hook); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	hook.Name = strings.TrimSpace(hook.Name)
	hook.Description = strings.TrimSpace(hook.Description)
	hook.Author = strings.TrimSpace(hook.Author)
	hook.Pattern = strings.TrimSpace(hook.Pattern)
	hook.MentionTo = strings.TrimSpace(hook.MentionTo)
	hook.Endpoint = strings.TrimSpace(hook.Endpoint)
	hook.Secret = strings.TrimSpace(hook.Secret)

	if hook.Name == "" {
		return false, c.JSON(http.StatusBadRequest, "Name must not be empty")
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
	if hook.Pattern == "" {
		return false, c.JSON(http.StatusBadRequest, "Pattern is invalid regular expression")
	}
	if _, err := regexp.Compile(hook.Pattern); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusBadRequest, "Pattern is invalid regular expression")
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
					_, err = relay.Publish(context.Background(), eev)
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
			Relay:   feedRelays[feedIndex%len(feedRelays)].Relay,
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

	go func() {
		from := time.Now()
		for {
			server(&from)
			time.Sleep(5 * time.Second)
		}
	}()

	manager()
}
