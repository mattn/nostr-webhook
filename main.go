package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/labstack/echo"
	_ "github.com/lib/pq"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

const name = "nostr-webhook"

const version = "0.0.0"

var revision = "HEAD"

var (
	feedRelay = "wss://relay-jp.nostr.wirednet.jp"

	postRelays = []string{
		"wss://nostr-relay.nokotaro.com",
		"wss://relay-jp.nostr.wirednet.jp",
		"wss://nostr.holybea.com",
		"wss://relay.snort.social",
		"wss://relay.damus.io",
		"wss://relay.nostrich.land",
		"wss://nostr.h3z.jp",
	}

	//go:embed static
	assets embed.FS
)

type entry struct {
	bun.BaseModel `bun:"table:entry,alias:e"`

	Name        string    `bun:"name,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description"`
	Author      string    `bun:"author,notnull" json:"author"`
	Pattern     string    `bun:"pattern,notnull" json:"pattern"`
	MentionTo   string    `bun:"mention_to,notnull" json:"mention_to"`
	Endpoint    string    `bun:"endpoint,notnull" json:"endpoint"`
	Secret      string    `bun:"secret,notnull,default:random_string(12)" json:"secret"`
	Enabled     bool      `bun:"enabled,notnull,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	re *regexp.Regexp
}

var hooks = []entry{}

func doHooks(ev *nostr.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		log.Println(err)
		return
	}
	for _, hook := range hooks {
		if !hook.re.MatchString(ev.Content) {
			continue
		}
		if hook.MentionTo != "" {
			found := false
			for _, tag := range ev.Tags {
				if tag.Key() != "p" {
					continue
				}
				if tag.Value() == hook.MentionTo {
					found = true
				}
			}
			if !found {
				return
			}
		}
		log.Printf("Matched entry %s", hook.Name)
		req, err := http.NewRequest(http.MethodPost, hook.Endpoint, bytes.NewReader(b))
		if err != nil {
			log.Println(err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+hook.Secret)
		go func(req *http.Request) {
			client := new(http.Client)
			client.Timeout = 15 * time.Second
			resp, err := client.Do(req)
			if err != nil {
				log.Println(err)
				return
			}
			defer resp.Body.Close()
			var eev nostr.Event
			err = json.NewDecoder(resp.Body).Decode(&eev)
			if err != nil {
				return
			}
			for _, r := range postRelays {
				relay, err := nostr.RelayConnect(context.Background(), r)
				if err != nil {
					log.Println(err)
					continue
				}
				_, err = relay.Publish(context.Background(), eev)
				if err != nil {
					log.Println(err)
				}
				relay.Close()
			}
		}(req)
	}
}

func reloadEntries(bundb *bun.DB) {
	err := bundb.NewSelect().Model((*entry)(nil)).Scan(context.Background(), &hooks)
	if err != nil {
		log.Println(err)
	}
	for i := range hooks {
		if hooks[i].Pattern != "" {
			hooks[i].re, err = regexp.Compile(hooks[i].Pattern)
			if err != nil {
				log.Fatal(err)
			}
		}
		if hooks[i].MentionTo != "" {
			_, npub, err := nip19.Decode(hooks[i].MentionTo)
			if err != nil {
				log.Fatal(err)
			}
			hooks[i].MentionTo = npub.(string)
		}
	}
	log.Printf("Reloaded %d entries", len(hooks))
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

	_, err = bundb.NewCreateTable().Model((*entry)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadEntries(bundb)

	log.Println("Connecting to relay")
	relay, err := nostr.RelayConnect(context.Background(), feedRelay)
	if err != nil {
		log.Println(err)
		return
	}
	defer relay.Close()

	log.Println("Connected to relay")

	events := make(chan *nostr.Event, 100)
	timestamp := nostr.Timestamp(from.Unix())
	filters := []nostr.Filter{{
		Kinds: []int{nostr.KindTextNote},
		Since: &timestamp,
	}}
	sub, err := relay.Subscribe(context.Background(), filters)
	if err != nil {
		log.Println(err)
		return
	}

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
				doHooks(ev)
				if ev.CreatedAt.Time().After(*from) {
					*from = ev.CreatedAt.Time()
				}
				retry = 0
			case <-time.After(time.Minute):
				reloadEntries(bundb)
			case <-time.After(10 * time.Second):
				if relay.ConnectionError != nil {
					log.Println(err)
					close(events)
					sub.Unsub()
					break events_loop
				}
				retry++
				log.Println("Health check", retry)
				if retry > 60 {
					close(events)
					sub.Unsub()
					break events_loop
				}
			}
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

func manager() {
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	bundb := bun.NewDB(db, pgdialect.New())
	defer bundb.Close()

	_, err = bundb.NewCreateTable().Model((*entry)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}

	e := echo.New()
	sub, _ := fs.Sub(assets, "static")
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(sub))))
	e.GET("/entries/:name", func(c echo.Context) error {
		var hook entry
		err = bundb.NewSelect().Model((*entry)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &hook)
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.POST("/entries/", func(c echo.Context) error {
		var hook entry
		err := c.Bind(&hook)
		if err == nil && hook.Name == "" {
			err = errors.New("name must not be empty")
		}
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		_, err = bundb.NewInsert().Model(&hook).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.POST("/entries/:name", func(c echo.Context) error {
		var hook entry
		err := c.Bind(&hook)
		if err == nil && hook.Name == "" {
			err = errors.New("name must not be empty")
		}
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		_, err = bundb.NewUpdate().Model(&hook).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.DELETE("/entries/:name", func(c echo.Context) error {
		_, err = bundb.NewDelete().Model((*entry)(nil)).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, c.Param("name"))
	})
	e.GET("/entries/:name", func(c echo.Context) error {
		var hook entry
		err := bundb.NewSelect().Model((*entry)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &hook)
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.GET("/entries", func(c echo.Context) error {
		var hooks []entry
		err = bundb.NewSelect().Model((*entry)(nil)).Order("created_at").Scan(context.Background(), &hooks)
		if err != nil {
			log.Println(err)
		}
		return c.JSON(http.StatusOK, hooks)
	})
	e.Logger.Fatal(e.Start(":8989"))
}

func main() {
	var ver bool
	flag.BoolVar(&ver, "v", false, "show version")
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
