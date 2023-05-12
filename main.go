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
	"github.com/robfig/cron/v3"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

const name = "nostr-webhook"

const version = "0.0.3"

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

type Hook struct {
	bun.BaseModel `bun:"table:hook,alias:e"`

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

type Task struct {
	bun.BaseModel `bun:"table:task,alias:t"`

	Name        string    `bun:"name,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description"`
	Author      string    `bun:"author,notnull" json:"author"`
	Spec        string    `bun:"cronspec,notnull" json:"spec"`
	Endpoint    string    `bun:"endpoint,notnull" json:"endpoint"`
	Secret      string    `bun:"secret,notnull,default:random_string(12)" json:"secret"`
	Enabled     bool      `bun:"enabled,notnull,default:false" json:"enabled"`
	CreatedAt   time.Time `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`

	id cron.EntryID
}

var (
	entriesMu sync.Mutex
	hooks     = []Hook{}
	cronsMu   sync.Mutex
	crons     = []Task{}

	jobs = cron.New()
)

func doEntries(ev *nostr.Event) {
	b, err := json.Marshal(ev)
	if err != nil {
		log.Println(err)
		return
	}
	entriesMu.Lock()
	defer entriesMu.Unlock()
	for _, entry := range hooks {
		if !entry.Enabled {
			continue
		}
		if !entry.re.MatchString(ev.Content) {
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
				return
			}
		}
		log.Printf("%v: Matched entry", entry.Name)
		req, err := http.NewRequest(http.MethodPost, entry.Endpoint, bytes.NewReader(b))
		if err != nil {
			log.Printf("%v: %v", entry.Name, err)
			continue
		}
		req.Header.Set("Authorization", "Bearer "+entry.Secret)
		go func(req *http.Request, name string) {
			client := new(http.Client)
			client.Timeout = 15 * time.Second
			resp, err := client.Do(req)
			if err != nil {
				log.Println(err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				log.Printf("%v: Invalid status code: %v", name, resp.StatusCode)
				return
			}
			var eev nostr.Event
			err = json.NewDecoder(resp.Body).Decode(&eev)
			if err != nil {
				log.Printf("%v: %v", name, err)
				return
			}
			for _, r := range postRelays {
				relay, err := nostr.RelayConnect(context.Background(), r)
				if err != nil {
					log.Printf("%v: %v: %v", name, r, err)
					continue
				}
				_, err = relay.Publish(context.Background(), eev)
				if err != nil {
					log.Printf("%v: %v: %v", name, r, err)
				}
				relay.Close()
			}
		}(req, entry.Name)
	}
}

func reloadTasks(bundb *bun.DB) {
	var cc []Task
	err := bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Scan(context.Background(), &cc)
	if err != nil {
		log.Println(err)
		return
	}

	jobs.Stop()
	for _, cr := range crons {
		jobs.Remove(cr.id)
	}
	for i := range cc {
		ct := cc[i]
		id, err := jobs.AddFunc(ct.Spec, func() {
			log.Printf("%v: Start", ct.Name)
			req, err := http.NewRequest(http.MethodGet, ct.Endpoint, nil)
			if err != nil {
				log.Printf("%v: %v", ct.Name, err)
				return
			}
			req.Header.Set("Authorization", "Bearer "+ct.Secret)
			client := new(http.Client)
			client.Timeout = 15 * time.Second
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
			for _, r := range postRelays {
				relay, err := nostr.RelayConnect(context.Background(), r)
				if err != nil {
					log.Printf("%v: %v: %v", ct.Name, r, err)
					continue
				}
				_, err = relay.Publish(context.Background(), eev)
				if err != nil {
					log.Printf("%v: %v: %v", ct.Name, r, err)
				}
				relay.Close()
			}
		})
		if err != nil {
			log.Printf("%v: Failed to add task: %v", ct.Name, err)
			continue
		}
		cc[i].id = id
	}
	jobs.Start()

	cronsMu.Lock()
	defer cronsMu.Unlock()
	crons = cc
	log.Printf("Reloaded %d crons", len(cc))
}

func reloadHooks(bundb *bun.DB) {
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

	entriesMu.Lock()
	defer entriesMu.Unlock()
	hooks = ee
	log.Printf("Reloaded %d hooks", len(ee))
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

	_, err = bundb.NewCreateTable().Model((*Hook)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadHooks(bundb)

	_, err = bundb.NewCreateTable().Model((*Task)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}
	reloadTasks(bundb)

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
				doEntries(ev)
				if ev.CreatedAt.Time().After(*from) {
					*from = ev.CreatedAt.Time()
				}
				retry = 0
			case <-time.After(time.Minute):
				reloadHooks(bundb)
				reloadTasks(bundb)
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

	_, err = bundb.NewCreateTable().Model((*Hook)(nil)).IfNotExists().Exec(context.Background())
	if err != nil {
		log.Println(err)
		return
	}

	e := echo.New()
	sub, _ := fs.Sub(assets, "static")
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(sub))))

	e.GET("/hooks/:name", func(c echo.Context) error {
		var hook Hook
		err = bundb.NewSelect().Model((*Hook)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &hook)
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.POST("/hooks/", func(c echo.Context) error {
		var hook Hook
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
	e.POST("/hooks/:name", func(c echo.Context) error {
		var hook Hook
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
	e.DELETE("/hooks/:name", func(c echo.Context) error {
		_, err = bundb.NewDelete().Model((*Hook)(nil)).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, c.Param("name"))
	})
	e.GET("/hooks/:name", func(c echo.Context) error {
		var hook Hook
		err := bundb.NewSelect().Model((*Hook)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &hook)
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, hook)
	})
	e.GET("/hooks", func(c echo.Context) error {
		var hooks []Hook
		err = bundb.NewSelect().Model((*Hook)(nil)).Order("created_at").Scan(context.Background(), &hooks)
		if err != nil {
			log.Println(err)
		}
		return c.JSON(http.StatusOK, hooks)
	})

	e.GET("/crons/:name", func(c echo.Context) error {
		var task Task
		err = bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &task)
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, task)
	})
	e.POST("/crons/", func(c echo.Context) error {
		var task Task
		err := c.Bind(&task)
		if err == nil && task.Name == "" {
			err = errors.New("name must not be empty")
		}
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		if _, err := cron.ParseStandard(task.Spec); err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		_, err = bundb.NewInsert().Model(&task).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, task)
	})
	e.POST("/crons/:name", func(c echo.Context) error {
		var task Task
		err := c.Bind(&task)
		if err == nil && task.Name == "" {
			err = errors.New("name must not be empty")
		}
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		_, err = bundb.NewUpdate().Model(&task).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, task)
	})
	e.DELETE("/crons/:name", func(c echo.Context) error {
		_, err = bundb.NewDelete().Model((*Task)(nil)).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, c.Param("name"))
	})
	e.GET("/crons/:name", func(c echo.Context) error {
		var task Task
		err := bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &task)
		if err != nil {
			log.Println(err)
			return c.String(http.StatusInternalServerError, err.Error())
		}
		return c.JSON(http.StatusOK, task)
	})
	e.GET("/crons", func(c echo.Context) error {
		var tasks []Task
		err = bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Scan(context.Background(), &tasks)
		if err != nil {
			log.Println(err)
		}
		return c.JSON(http.StatusOK, tasks)
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
