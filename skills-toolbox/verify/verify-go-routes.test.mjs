import assert from 'node:assert/strict'
import test from 'node:test'

import { goRoutes, routeChanges } from './verify-go-routes.mjs'

test('net/http reads method patterns, host paths and ANY without reading noise', () => {
  const routes = goRoutes(`package api
import "net/http"
func setup() {
  mux.Handle("POST /claims/{id}", auth.Require(handler))
  mux.HandleFunc("example.com/x", serve)
  mux.HandleFunc("/plain", serve)
  // mux.Handle("GET /comment", nope)
  s := "mux.Handle(\"GET /string\", nope)"
}`)
  assert.deepEqual(
    routes.map(({ method, path, auth }) => ({ method, path, auth })),
    [
      { method: 'POST', path: '/claims/{id}', auth: 'Require' },
      { method: 'ANY', path: 'example.com/x', auth: 'not detected' },
      { method: 'ANY', path: '/plain', auth: 'not detected' },
    ],
  )
  assert.equal(routes[0].line, 4)
})

test('chi reads verbs, With, enclosing Use, nested routes and mounts', () => {
  const routes = goRoutes(`package api
import "github.com/go-chi/chi/v5"
func setup() {
 r.Route("/v1", func(r chi.Router) {
   r.Use(session.Check)
   s := "{"
   r.Post("/claims", create)
   r.Route("/nested", func(r chi.Router) {
     r.With(jwt.Verify).Get("/item", get)
   })
 })
 r.Method("PATCH", "/item", patch)
 r.Mount("/admin", admin)
}`)
  assert.deepEqual(
    routes.map((r) => [r.method, r.path, r.auth]),
    [
      ['POST', '/v1/claims', 'Check'],
      ['GET', '/v1/nested/item', 'Verify'],
      ['PATCH', '/item', 'not detected'],
      ['ANY', '/admin/*', 'not detected'],
    ],
  )
  assert.match(routes.at(-1).handler, /mounted router/)
})

test('named router helpers omit registrations with unknown caller prefixes', () => {
  const routes = goRoutes(`package api
import (
 "github.com/go-chi/chi/v5"
 "github.com/gin-gonic/gin"
 "net/http"
)
func setup() {
 r.Route("/v1", apiRoutes)
 r.Get("/root", serve)
 r.Route("/known", func(r chi.Router) { r.Get("/item", serve) })
}
func apiRoutes(r chi.Router) { r.Get("/chi-item", serve) }
func (s *server) mountedRoutes(r chi.Router) { r.Get("/method-item", serve) }
func ginRoutes(r *gin.RouterGroup) { r.GET("/gin-item", serve) }
func httpRoutes(mux *http.ServeMux) { mux.Handle("/http-item", serve) }
`)
  assert.deepEqual(
    routes.map((r) => r.path),
    ['/root', '/known/item'],
  )
})

test('gin reads verbs, Any, Handle and chained group middleware', () => {
  const routes = goRoutes(`package api
import "github.com/gin-gonic/gin"
func setup() {
 v := r.Group("/v1", auth.Required)
 nested := v.Group("/private")
 nested.GET("/item", get)
 v.Any("/all", serve)
 r.Handle("DELETE", "/item", remove)
 r.POST("/login", session.Check, login)
}`)
  assert.deepEqual(
    routes.map((r) => [r.method, r.path, r.auth]),
    [
      ['GET', '/v1/private/item', 'Required'],
      ['ANY', '/v1/all', 'Required'],
      ['DELETE', '/item', 'not detected'],
      ['POST', '/login', 'Check'],
    ],
  )
})

test('imports gate frameworks; variable paths and comment/string registrations are ignored', () => {
  assert.deepEqual(goRoutes('package api\nfunc f() { cache.Get("/key", x) }'), [])
  assert.deepEqual(goRoutes('package api\nimport "net/http"\nfunc f() { mux.Handle(path, h) }'), [])
})

test('route changes report rewiring and preserve moves', () => {
  const route = { method: 'GET', path: '/x', handler: 'serve', auth: 'not detected', line: 1 }
  assert.deepEqual(routeChanges([route], [{ ...route, line: 20 }]), [])
  assert.deepEqual(
    routeChanges([route], [{ ...route, handler: 'other' }]).map((r) => r.change),
    ['changed'],
  )
  assert.deepEqual(
    routeChanges([], [route]).map((r) => r.change),
    ['added'],
  )
  assert.deepEqual(
    routeChanges([route], []).map((r) => r.change),
    ['removed'],
  )
})

test('supported framework verbs and literal forms retain bounds and honest auth labels', () => {
  const source = `package api
import (
 "github.com/go-chi/chi/v5"
 "github.com/gin-gonic/gin"
)
func setup() {
 ${['Get', 'Head', 'Post', 'Put', 'Patch', 'Delete', 'Options'].map((m) => `r.${m}("/${m}", serve)`).join('\n')}
 r.MethodFunc("GET", "/method", serve)
 r.HandleFunc("/any", serve)
 ${['GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS'].map((m) => `g.${m}("/${m}", serve)`).join('\n')}
 g.GET("/${'x'.repeat(200)}", ${'Require'.repeat(12)}(serve))
 g.GET("/unknown", auth.none(serve))
 g.GET("/login", login)
}`
  const routes = goRoutes(source)
  assert.equal(routes.length, 19)
  assert.equal(routes.at(-3).path.length, 201)
  assert.equal(routes.at(-3).auth.length, 40)
  assert.ok(routes.at(-3).path.endsWith('x'))
  assert.equal(routes.at(-2).auth, 'not detected')
  assert.equal(routes.at(-1).auth, 'not detected')
  assert.ok(routes.every((r) => r.auth !== 'none'))
})

test('Use on one router does not label sibling routers; string braces and noise are inert', () => {
  const routes = goRoutes(`package api
import "github.com/go-chi/chi/v5"
func setup() {
 a.Use(jwt.Check)
 b.Get("/public", public)
 a.Get("/private", private)
 /* b.Get("/comment", ignored) */
 raw := \`b.Get("/string", ignored)\`
}`)
  assert.deepEqual(
    routes.map((r) => r.auth),
    ['not detected', 'Check'],
  )
})

test('chi Groups preserve prefixes and With chains retain their receiver', () => {
  const routes = goRoutes(`package api
import "github.com/go-chi/chi/v5"
func setup() {
 r.Route("/v1", func(r chi.Router) {
   r.Group(func(g chi.Router) {
     g.Use(auth.Require)
     g.Get("/group", serve)
   })
   r.With(jwt.Verify).With(trace).Get("/chain", serve)
 })
}`)
  assert.deepEqual(
    routes.map((r) => [r.path, r.auth]),
    [
      ['/v1/group', 'Require'],
      ['/v1/chain', 'Verify'],
    ],
  )
})

test('mixed imports preserve net/http patterns alongside gin Handle', () => {
  const routes = goRoutes(`package api
import (
 "net/http"
 "github.com/gin-gonic/gin"
)
func setup() {
 mux.Handle("POST /http", serve)
 g.Handle("GET", "/gin", serve)
}`)
  assert.deepEqual(
    routes.map((r) => [r.method, r.path]),
    [
      ['POST', '/http'],
      ['GET', '/gin'],
    ],
  )
})

test('auth labels come from wrappers rather than names inside handler bodies', () => {
  const routes = goRoutes(`package api
import "net/http"
func setup() {
 mux.HandleFunc("/inline", func(w http.ResponseWriter, r *http.Request) { session.Read(r) })
 mux.Handle("/wrapped", auth.Require(serve))
}`)
  assert.deepEqual(
    routes.map((r) => r.auth),
    ['not detected', 'Require'],
  )
})

test('route identity retains long suffixes until display bounding', () => {
  const parse = (suffix) =>
    goRoutes(`package api
import "net/http"
func setup() { mux.Handle("GET /${'x'.repeat(180)}${suffix}", serve) }`)
  assert.deepEqual(
    routeChanges(parse('a'), parse('b')).map((r) => r.change),
    ['added', 'removed'],
  )
  assert.equal(routeChanges([], [...parse('a'), ...parse('b')]).length, 2)
})

test('chi With middleware survives Route and Group callbacks', () => {
  const source = `package api
import "github.com/go-chi/chi/v5"
func setup() {
 r.With(jwt.Verify).Route("/v1", func(r chi.Router) { r.Get("/item", serve) })
 r.With(session.Check).Group(func(g chi.Router) { g.Get("/group", serve) })
}`
  assert.deepEqual(
    goRoutes(source).map((r) => [r.path, r.auth]),
    [
      ['/v1/item', 'Verify'],
      ['/group', 'Check'],
    ],
  )
  assert.equal(
    routeChanges(goRoutes(source), goRoutes(source.replace('jwt.Verify', 'auth.Require')))[0]
      .change,
    'changed',
  )
})

test('endpoint constructors are not evidence of authentication', () => {
  const routes = goRoutes(`package api
import "net/http"
func setup() {
 mux.Handle("/login", handlers.Login(svc))
 mux.Handle("/token", tokenHandler(store))
 mux.Handle("/authors", authors.List(db))
 mux.Handle("/auth", authRoutes())
 mux.Handle("/wrapped", auth.Require(handler))
}`)
  assert.deepEqual(
    routes.map((r) => r.auth),
    ['not detected', 'not detected', 'not detected', 'not detected', 'Require'],
  )
})

test('non-auth wrappers preserve detected auth from nested handler wrappers', () => {
  const routes = goRoutes(`package api
import "net/http"
func setup() {
 mux.Handle("/timeout", http.TimeoutHandler(auth.Require(handler), time.Second, "timeout"))
 mux.Handle("/nested", recoverer(http.TimeoutHandler(jwt.Verify(serve), time.Second, "timeout")))
 mux.Handle("/outer", session.Check(auth.Require(handler)))
 mux.Handle("/constructor", http.TimeoutHandler(handlers.Login(svc), time.Second, "timeout"))
}`)
  assert.deepEqual(
    routes.map((r) => r.auth),
    ['Require', 'Verify', 'Check', 'not detected'],
  )
})

test('framework verbs reject client URLs and relative keys', () => {
  const routes = goRoutes(`package api
import (
 "net/http"
 "github.com/go-chi/chi/v5"
)
func setup() {
 http.Get("https://example.com/x")
 q.Get("a/b", value)
 r.Get("/server", serve)
 mux.Handle("example.com/host", serve)
}`)
  assert.deepEqual(
    routes.map((r) => r.path),
    ['/server', 'example.com/host'],
  )
})

test('gin group selectors, assignments and call chains retain prefixes', () => {
  const routes = goRoutes(`package api
import "github.com/gin-gonic/gin"
func setup() {
 v := s.router.Group("/v1", auth.Required)
 v.GET("/item", serve)
 v = r.Group("/v2")
 v.GET("/item", serve)
 var w = r.Group("/v3")
 w.GET("/item", serve)
 r.Group("/v4").GET("/item", serve)
 x := r.Group("/v5").Group("/nested")
 x.GET("/item", serve)
}`)
  assert.deepEqual(
    routes.map((r) => r.path),
    ['/v1/item', '/v2/item', '/v3/item', '/v4/item', '/v5/nested/item'],
  )
  assert.equal(routes[0].auth, 'Required')
})
