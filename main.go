package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
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

type EnvRequiredError struct {
	Env string
}

func main() {
	if err := execute(); err != nil {
		log.Fatal(err)
	}
}

func execute() error {
	port := ":" + getenvWithDefault("PORT", "8080")

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

	if err := http.ListenAndServe(port, c); err != nil {
		return fmt.Errorf("serving: %w", err)
	}

	return nil
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
		return nil, fmt.Errorf("parsing template: %w", err)
	}

	c := &Controller{
		templates: templates,
		username:  username,
		password:  password,
		redis:     rc,
	}

	r := httprouter.New()

	r.GET("/", c.Index)
	r.GET("/ads.txt", c.AdsTxt)
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
	http.Redirect(w, r, "https://blog.web-apps.tech", http.StatusFound)
}

// ads.txt for Google AdSense
func (c *Controller) AdsTxt(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	http.Redirect(w, r, "https://blog.web-apps.tech/ads.txt", http.StatusFound)
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

type PostColorRequest struct {
	NumOfMember int      `json:"num_of_member"`
	Colors      []string `json:"colors"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (errResp ErrorResponse) WriteTo(w io.Writer) (int64, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(errResp); err != nil {
		log.Print(err)
	}

	return buf.WriteTo(w)
}

func (c *Controller) PostColor(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
	var postColorRequest PostColorRequest
	if err := json.NewDecoder(r.Body).Decode(&postColorRequest); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		ErrorResponse{Error: err.Error(), Message: "cannot parse request body as json"}.WriteTo(w)
		return
	}

	if err := c.redis.Set(r.Context(), "numOfMember", strconv.Itoa(postColorRequest.NumOfMember), 0).Err(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		ErrorResponse{Error: err.Error(), Message: "cannot save # of member into redis"}.WriteTo(w)
		return
	}

	for i := 0; i < postColorRequest.NumOfMember; i++ {
		nthColor := postColorRequest.Colors[i]
		if nthColor == "" {
			w.WriteHeader(http.StatusBadRequest)
			ErrorResponse{Error: "", Message: fmt.Sprintf("%d-th of color is not selected", i+1)}.WriteTo(w)
			return
		}

		if err := c.redis.Set(r.Context(), strconv.Itoa(i), nthColor, 0).Err(); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			ErrorResponse{Error: err.Error(), Message: "cannot save %d-th of member into redis"}.WriteTo(w)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{}`))
	return
}

func MustSubFS(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}

func ErrEnvRequired(name string) error {
	return EnvRequiredError{
		Env: name,
	}
}

func (err EnvRequiredError) Error() string {
	return fmt.Sprintf("environment variable %s is required", err.Env)
}

func ErrorQuery(errorMessage string) string {
	return "?error=" + url.QueryEscape(errorMessage)
}

func getenvWithDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}

	return defaultVal
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
