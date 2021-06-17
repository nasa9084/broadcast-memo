package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/go-redis/redis/v8"
	"github.com/julienschmidt/httprouter"
)

var (
	//go:embed amongus/template/*.tmpl
	templateFS embed.FS
	//go:embed amongus/img
	colorImages embed.FS
)

var colors = []string{
	"red",
	"blue",
	"green",
	"pink",
	"orange",
	"yellow",
	"black",
	"white",
	"purple",
	"brown",
	"cyan",
	"lime",
	"maroon",
	"rose",
	"banana",
	"gray",
	"tan",
	"coral",
}

const (
	minNumOfMember = 4
	maxNumOfMember = 15
)

func main() {
	if err := execute(); err != nil {
		log.Fatal(err)
	}
}

func execute() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	port = ":" + port

	if os.Getenv("REDIS_URL") == "" {
		return ErrEnvRequired("REDIS_URL")
	}

	redisURL, err := url.Parse(os.Getenv("REDIS_URL"))
	if err != nil {
		return fmt.Errorf("parsing $REDIS_URL: %w", err)
	}

	redisPassword, _ := redisURL.User.Password()
	redisOptions := redis.Options{
		Addr:     redisURL.Host,
		Password: redisPassword,
	}

	username := os.Getenv("USERNAME")
	if username == "" {
		return ErrEnvRequired("USERNAME")
	}

	password := os.Getenv("PASSWORD")
	if password == "" {
		return ErrEnvRequired("PASSWORD")
	}

	c, err := NewController(username, password, &redisOptions)
	if err != nil {
		return fmt.Errorf("initializing controller: %w", err)
	}

	log.Printf("Listening on %s", port)
	return http.ListenAndServe(port, c)
}

type Controller struct {
	http.Handler

	templates *template.Template

	username, password string
	redis              *redis.Client
}

func NewController(username, password string, redisOption *redis.Options) (*Controller, error) {
	rc := redis.NewClient(redisOption)

	templateFuncMap := template.FuncMap{
		"incr": incr,
	}

	templates, err := template.New("").Funcs(templateFuncMap).ParseFS(templateFS, "amongus/template/*")
	if err != nil {
		return nil, err
	}

	c := &Controller{
		templates: templates,
		username:  username,
		password:  password,
		redis:     rc,
	}

	r := httprouter.New()

	r.GET("/", c.Index)
	r.GET("/overlay", c.Overlay)
	r.GET("/select", c.ColorSelectPage)
	r.POST("/color", c.PostColor)

	r.ServeFiles("/img/*filepath", http.FS(MustSubFS(colorImages, "amongus/img")))

	c.Handler = r
	return c, nil
}

func (c *Controller) auth(w http.ResponseWriter, r *http.Request) bool {
	// very simple basic auth but enough
	username, password, ok := r.BasicAuth()
	if !ok {
		w.Header().Add("www-authenticate", "Basic")
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}

	if username != c.username || password != c.password {
		w.WriteHeader(http.StatusForbidden)
		return false
	}

	return true
}

func (c *Controller) Index(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	http.Redirect(w, r, "/select", http.StatusFound)
}

func (c *Controller) Overlay(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	n, err := c.redis.Get(r.Context(), "numOfMember").Int()
	if err != nil {
		http.Redirect(w, r, "/select"+ErrorQuery("numOfMember has not been set"), http.StatusFound)
		return
	}

	colors := make([]string, 0, n)
	for i := 0; i < n; i++ {
		color := c.redis.Get(r.Context(), strconv.Itoa(i)).Val()
		if color == "" {
			http.Redirect(w, r, "/select"+ErrorQuery(fmt.Sprintf("%d-th of color has not been set", i)), http.StatusFound)
			return
		}
		colors = append(colors, color)
	}

	var buf bytes.Buffer
	if err := c.templates.Lookup("overlay.html.tmpl").Execute(&buf, colors); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)

	if _, err := buf.WriteTo(w); err != nil {
		log.Println(err)
	}
}

type colorSelectPageData struct {
	NumOfMember         []int
	MemberIndex         []int
	SelectedNumOfMember int
	Colors              []string
	NthColors           []string

	Error string
}

func (c *Controller) ColorSelectPage(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	if !c.auth(w, r) {
		return
	}

	errorMessage, err := url.QueryUnescape(r.URL.Query().Get("error"))
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	currentNumOfMember, err := c.redis.Get(r.Context(), "numOfMember").Int()
	if err != nil {
		if !errors.Is(err, redis.Nil) {
			log.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	nthColors := make([]string, maxNumOfMember)
	for i := 0; i < maxNumOfMember; i++ {
		nthColor := c.redis.Get(r.Context(), strconv.Itoa(i)).Val()
		nthColors[i] = nthColor
	}

	data := colorSelectPageData{
		NumOfMember:         getNumOfMemberList(),
		MemberIndex:         getMemberIndex(),
		SelectedNumOfMember: currentNumOfMember,
		Colors:              colors,
		NthColors:           nthColors,
		Error:               errorMessage,
	}

	var buf bytes.Buffer
	if err := c.templates.Lookup("select.html.tmpl").Execute(&buf, data); err != nil {
		log.Printf("ERROR on executing template: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)

	if _, err := buf.WriteTo(w); err != nil {
		log.Println(err)
	}
}

func (c *Controller) PostColor(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	redirectTo := "/select"
	defer func() {
		http.Redirect(w, r, redirectTo, http.StatusFound)
	}()

	numOfMember, err := strconv.Atoi(r.FormValue("numOfMember"))
	if err != nil {
		redirectTo += ErrorQuery("numOfMember cannot be parsed as int")
		return
	}

	if err := c.redis.Set(r.Context(), "numOfMember", numOfMember, 0).Err(); err != nil {
		redirectTo += ErrorQuery(err.Error())
		return
	}

	for i := 0; i < numOfMember; i++ {
		nthColor := r.FormValue(strconv.Itoa(i))
		if nthColor == "" {
			redirectTo += ErrorQuery(fmt.Sprintf("%d-th of color is not selected", i+1))
			return
		}
		if err := c.redis.Set(r.Context(), strconv.Itoa(i), nthColor, 0).Err(); err != nil {
			redirectTo += ErrorQuery(err.Error())
			return
		}
	}
}

func MustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}

func ErrEnvRequired(name string) error {
	return fmt.Errorf("environment variable %s is required", name)
}

func ErrorQuery(errorMessage string) string {
	return "?error=" + url.QueryEscape(errorMessage)
}

func getNumOfMemberList() []int {
	list := make([]int, 0, maxNumOfMember-minNumOfMember+1)

	for i := minNumOfMember; i <= maxNumOfMember; i++ {
		list = append(list, i)
	}

	return list
}

func getMemberIndex() []int {
	list := make([]int, 0, maxNumOfMember)

	for i := 0; i < maxNumOfMember; i++ {
		list = append(list, i)
	}

	return list
}

func incr(a int) int {
	return a + 1
}
