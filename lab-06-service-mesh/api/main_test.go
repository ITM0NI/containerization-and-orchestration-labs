package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type orderDriver struct{}
type orderConn struct{}
type orderRows struct{ done bool }

var inserts int

func (orderDriver) Open(string) (driver.Conn, error)  { return orderConn{}, nil }
func (orderConn) Prepare(string) (driver.Stmt, error) { panic("unexpected prepare") }
func (orderConn) Close() error                        { return nil }
func (orderConn) Begin() (driver.Tx, error)           { panic("unexpected transaction") }
func (orderConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	inserts++
	return &orderRows{}, nil
}
func (*orderRows) Columns() []string {
	return []string{"id", "item", "created_at", "processed", "processed_at"}
}
func (*orderRows) Close() error { return nil }
func (r *orderRows) Next(v []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(v, []driver.Value{int64(1), "tea", time.Now(), false, nil})
	return nil
}
func init() { sql.Register("orders-test", orderDriver{}) }

func TestVersion(t *testing.T) {
	for _, v := range []string{"v1", "v2"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("VERSION", v)
			w := httptest.NewRecorder()
			version(w, httptest.NewRequest("GET", "/version", nil))
			if w.Code != 200 || strings.TrimSpace(w.Body.String()) != v {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestFailV2(t *testing.T) {
	t.Setenv("VERSION", "v2")
	t.Setenv("FAIL", "true")
	t.Setenv("FLAKY", "false")
	db, _ := sql.Open("orders-test", "")
	defer db.Close()
	inserts = 0
	w := httptest.NewRecorder()
	createOrder(db)(w, httptest.NewRequest("POST", "/order", strings.NewReader(`{"item":"tea"}`)))
	if w.Code != 500 || inserts != 0 {
		t.Fatalf("code=%d inserts=%d", w.Code, inserts)
	}
	w = httptest.NewRecorder()
	health(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}
func TestFlakyV1(t *testing.T) {
	t.Setenv("VERSION", "v1")
	t.Setenv("FLAKY", "true")
	t.Setenv("FAIL", "true")
	old := randomFloat
	defer func() { randomFloat = old }()
	db, _ := sql.Open("orders-test", "")
	defer db.Close()
	for _, tc := range []struct {
		draw         float64
		code, writes int
	}{{0.249, 500, 0}, {0.25, 201, 1}, {0.99, 201, 1}} {
		inserts = 0
		randomFloat = func() float64 { return tc.draw }
		w := httptest.NewRecorder()
		createOrder(db)(w, httptest.NewRequest("POST", "/order", strings.NewReader(`{"item":"tea"}`)))
		if w.Code != tc.code || inserts != tc.writes {
			t.Fatalf("draw=%g code=%d inserts=%d", tc.draw, w.Code, inserts)
		}
	}
}
func TestInvalidOrderStill400(t *testing.T) {
	t.Setenv("VERSION", "v2")
	t.Setenv("FAIL", "true")
	for _, body := range []string{`not-json`, `{"item":"  "}`} {
		w := httptest.NewRecorder()
		createOrder(nil)(w, httptest.NewRequest("POST", "/order", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
