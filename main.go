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
	"net/http"
	"net/url"
	"os"
	"regexp"
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
)

const name = "nostr-webhook"

const version = "0.0.34"

var revision = "HEAD"

var (
	feedRelays = []string{
		"wss://relay-jp.nostr.wirednet.jp",
		"wss://nostr-relay.nokotaro.com",
	}
	feedIndex = 0

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

// Hook is struct for webhook
type Hook struct {
	bun.BaseModel `bun:"table:hook,alias:e"`

	Name        string    `bun:"name,pk,notnull" json:"name"`
	Description string    `bun:"description,notnull" json:"description"`
	Author      string    `bun:"author,notnull" json:"author"`
	Pattern     string    `bun:"pattern,notnull" json:"pattern"`
	MentionTo   string    `bun:"mention_to,notnull" json:"mention_to"`
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

// Info is struct for /info
type Info struct {
	Relay   string `json:"relay"`
	Version string `json:"version"`
}

var (
	hooksMu sync.Mutex
	hooks   = []Hook{}
	tasksMu sync.Mutex
	tasks   = []Task{}

	jobs *cron.Cron
)

func doEntries(ev *nostr.Event) {
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
		ee[i].id = id
	}
	jobs.Start()

	tasksMu.Lock()
	defer tasksMu.Unlock()
	tasks = ee
	log.Printf("Reloaded %d tasks", len(ee))
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
	relay, err := nostr.RelayConnect(context.Background(), feedRelays[feedIndex%len(feedRelays)])
	if err != nil {
		feedIndex++
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

func jwtUser(c echo.Context) (string, error) {
	cookie, err := c.Request().Cookie("CF_Authorization")
	if err != nil {
		return "unknown", nil
	}

	claims := jwt.MapClaims{}
	parser := new(jwt.Parser)
	_, _, err = parser.ParseUnverified(cookie.Value, &claims)
	if err != nil {
		return "", err
	}
	if email, ok := map[string]interface{}(claims)["email"]; ok {
		return email.(string), nil
	}
	return "unknown", nil
}

func checkHook(c echo.Context, hook *Hook) (bool, error) {
	if err := c.Bind(&hook); err != nil {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
	}
	if hook.Name == "" {
		return false, c.JSON(http.StatusBadRequest, "Name must not be empty")
	}
	if hook.Endpoint == "" {
		return false, c.JSON(http.StatusBadRequest, "Endpoint must not be empty")
	}
	if name, err := jwtUser(c); err == nil {
		hook.Author = name
	} else {
		log.Println(err)
		return false, c.JSON(http.StatusInternalServerError, err.Error())
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
	if task.Name == "" {
		return false, c.JSON(http.StatusBadRequest, "Name must not be empty")
	}
	if task.Endpoint == "" {
		return false, c.JSON(http.StatusBadRequest, "Endpoint must not be empty")
	}
	if name, err := jwtUser(c); err == nil {
		task.Author = name
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
	e.Use(middleware.Logger())
	e.IPExtractor = echo.ExtractIPFromXFFHeader()

	sub, _ := fs.Sub(assets, "static")
	e.GET("/*", echo.WrapHandler(http.FileServer(http.FS(sub))))

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
		err := bundb.NewSelect().Model((*Hook)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &hook)
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
		_, err := bundb.NewUpdate().Model(&hook).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
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
		err := bundb.NewSelect().Model((*Task)(nil)).Order("created_at").Limit(1).Scan(context.Background(), &task)
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
		_, err = bundb.NewUpdate().Model(&task).Where("name = ?", c.Param("name")).Exec(context.Background())
		if err != nil {
			e.Logger.Error(err)
			return c.JSON(http.StatusInternalServerError, err.Error())
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

	e.GET("/info", func(c echo.Context) error {
		info := Info{
			Relay:   feedRelays[feedIndex%len(feedRelays)],
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
