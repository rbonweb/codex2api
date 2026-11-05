package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestQualityTestJobTimeoutBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	h := &Handler{db: db, store: store}
	parent, stop := context.WithCancel(context.Background())
	h.StartQualityTests(parent)
	defer func() { stop(); h.WaitQualityTests() }()
	router := gin.New()
	router.POST("/accounts/:id/quality-test", h.CreateQualityTestJob)
	for _, tc := range []struct {
		body     string
		wantCode int
		wantMS   int64
	}{
		{`{"model":"gpt-4o-mini","prompt":"HTML","timeout_minutes":40}`, http.StatusBadRequest, 0},
		{`{"model":"gpt-4o-mini","prompt":"HTML","timeout_minutes":2}`, http.StatusBadRequest, 0},
		{`{"model":"gpt-4o-mini","prompt":"HTML","timeout_minutes":30}`, http.StatusAccepted, (30 * time.Minute).Milliseconds()},
		{`{"model":"gpt-4o-mini","prompt":"HTML"}`, http.StatusAccepted, database.QualityTestDefaultTimeout.Milliseconds()},
	} {
		id, err := db.InsertAccountWithCredentials(context.Background(), "account", map[string]interface{}{"access_token": "test-key", "models": []string{"gpt-4o-mini"}}, "")
		if err != nil {
			t.Fatal(err)
		}
		store.AddAccount(&auth.Account{DBID: id, AccessToken: "test-key", Models: []string{"gpt-4o-mini"}, PlanType: "pro", Status: auth.StatusReady, ProxyURL: "http://127.0.0.1:1"})
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/accounts/%d/quality-test", id), strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != tc.wantCode {
			t.Fatalf("%s: code=%d %s", tc.body, response.Code, response.Body.String())
		}
		if tc.wantCode != http.StatusAccepted {
			continue
		}
		var body struct {
			Job database.QualityTestJob `json:"job"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		stored, err := db.GetQualityTestJob(context.Background(), body.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if body.Job.TimeoutMS != tc.wantMS || stored.TimeoutMS != tc.wantMS {
			t.Fatalf("%s: timeout created=%d stored=%d want=%d", tc.body, body.Job.TimeoutMS, stored.TimeoutMS, tc.wantMS)
		}
	}
}
