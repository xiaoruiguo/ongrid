// package main

// import (
//     "net/http"
//     "github.com/go-chi/chi/v5"
//     "github.com/go-chi/chi/v5/middleware"
// )

// func main() {
//     r := chi.NewRouter()
//     r.Use(middleware.Logger) // 使用日志中间件
// 	r.Use(middleware.Recoverer) // 使用恢复中间件

//     r.Get("/", func(w http.ResponseWriter, r *http.Request) {
//         w.Write([]byte("welcome"))
//     })

//     http.ListenAndServe(":3000", r)
// }

package main

import (
    "fmt"
    "net/http"
    "encoding/json"

    "github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type Server struct {
    Router *chi.Mux
}

func CreateServer() *Server {
    server := &Server{
        Router: chi.NewRouter(),
    }
    return server
}

func Greet(w http.ResponseWriter, r *http.Request) {
    w.Write([]byte("Hello, World!!"))
}

// func (server *Server) MountHandlers() {
//     server.Router.Get("/greet", Greet)
// }

func (server *Server) MountHandlers() {
    server.Router.Get("/greet", Greet)

    todosRouter := chi.NewRouter()
    todosRouter.Group(func(r chi.Router) {
        r.Get("/", GetTodos)
        r.Post("/", AddTodo)
    })

    server.Router.Mount("/todos", todosRouter)
}

type Todo struct {
    Task      string `json:"task"`
    Completed bool   `json:"completed"`
}

var Todos []*Todo

func AddTodo(w http.ResponseWriter, r *http.Request) {
    todo := new(Todo)
    if err := json.NewDecoder(r.Body).Decode(todo); err != nil {
        w.WriteHeader(http.StatusBadRequest)
        w.Write([]byte("Please enter a correct Todo!!"))
        return
    }
    Todos = append(Todos, todo)
    w.WriteHeader(http.StatusOK)
    w.Write([]byte("Todo added!!"))
}

func GetTodos(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(Todos)
}

func main() {
    server := CreateServer()
	server.Router.Use(middleware.Logger) // 使用日志中间件
	server.Router.Use(middleware.Recoverer) // 使用恢复中间件
	server.MountHandlers()

    fmt.Println("server running on port:5000")
    http.ListenAndServe(":5000", server.Router)
}