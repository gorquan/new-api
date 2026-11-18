package router

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestListModelsSupportsOpenAIAndGeminiAuthentication(t *testing.T) {
	setupRelayRouterTestDB(t)

	user := model.User{
		Username: "models-user",
		Status:   common.UserStatusEnabled,
		Group:    "default",
		Quota:    100,
	}
	require.NoError(t, model.DB.Create(&user).Error)
	require.NoError(t, model.DB.Create(&model.Token{
		UserId:         user.Id,
		Key:            "modelstestkey",
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}).Error)

	engine := gin.New()
	SetRelayRouter(engine)

	tests := []struct {
		name           string
		path           string
		headerName     string
		expectedObject string
		expectedField  string
	}{
		{
			name:           "OpenAI bearer token",
			path:           "/v1/models",
			headerName:     "Authorization",
			expectedObject: "list",
			expectedField:  "data",
		},
		{
			name:          "Gemini API key header",
			path:          "/v1/models",
			headerName:    "x-goog-api-key",
			expectedField: "models",
		},
		{
			name:          "Gemini API key query",
			path:          "/v1/models?key=modelstestkey",
			expectedField: "models",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.headerName != "" {
				value := "modelstestkey"
				if test.headerName == "Authorization" {
					value = "Bearer " + value
				}
				request.Header.Set(test.headerName, value)
			}

			engine.ServeHTTP(recorder, request)

			require.Equal(t, http.StatusOK, recorder.Code)
			var payload map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &payload))
			assert.Contains(t, payload, test.expectedField)
			assert.NotContains(t, payload, "error")
			if test.expectedObject != "" {
				assert.Equal(t, test.expectedObject, payload["object"])
			}
		})
	}
}

func TestImageSVGAuthorBilling(t *testing.T) {
	setupRelayRouterTestDB(t)
	require.NoError(t, i18n.Init())
	ratio_setting.InitRatioSettings()
	service.InitHttpClient()
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Log{}, &model.Option{}, &model.QuotaData{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}))
	logging, batch, cache := common.LogConsumeEnabled, common.BatchUpdateEnabled, common.MemoryCacheEnabled
	common.LogConsumeEnabled, common.BatchUpdateEnabled, common.MemoryCacheEnabled = true, false, false
	t.Cleanup(func() {
		common.LogConsumeEnabled, common.BatchUpdateEnabled, common.MemoryCacheEnabled = logging, batch, cache
	})
	saved := map[string]string{}
	require.NoError(t, config.GlobalConfig.SaveToDB(func(key, value string) error { saved[key] = value; return nil }))
	selfUse := operation_setting.SelfUseModeEnabled
	operation_setting.SelfUseModeEnabled = true
	t.Cleanup(func() {
		operation_setting.SelfUseModeEnabled = selfUse
		require.NoError(t, config.GlobalConfig.LoadFromDB(saved))
	})
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		"billing_setting.billing_mode": `{"gpt-image-2.6-flare":"tiered_expr","gpt-5.4-mini":"tiered_expr","expensive-author":"tiered_expr"}`,
		"billing_setting.billing_expr": `{"gpt-image-2.6-flare":"tier(\"image\", p*5+c*30)","gpt-5.4-mini":"tier(\"author\", p*0.75+c*4.5)","expensive-author":"tier(\"author\",fixed(201))"}`,
	}))
	const response = `{"data":[{"b64_json":"PHN2Zy8+"}],"usage":{"input_tokens":89,"output_tokens":824},"svg_usage":{"model":"gpt-5.4-mini-2026-03-17","input_tokens":1599,"output_tokens":155,"reasoning_tokens":80,"total_tokens":1754}}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, response)
	}))
	t.Cleanup(upstream.Close)
	channel := model.Channel{Type: constant.ChannelTypeOpenAI, Name: "image", Key: "test", Status: common.ChannelStatusEnabled, Models: "gpt-image-2.6-flare", Group: "default", BaseURL: &upstream.URL}
	require.NoError(t, channel.Insert())
	user := model.User{Username: "image-test", Status: common.UserStatusEnabled, Group: "default", Quota: 100000000}
	require.NoError(t, model.DB.Create(&user).Error)
	token := model.Token{UserId: user.Id, Key: "svgtest", Status: common.TokenStatusEnabled, Group: "default", ExpiredTime: -1, UnlimitedQuota: true}
	require.NoError(t, model.DB.Create(&token).Error)
	engine := gin.New()
	SetRelayRouter(engine)
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer svgtest")
		resp := httptest.NewRecorder()
		engine.ServeHTTP(resp, req)
		return resp
	}
	const request = `{"model":"gpt-image-2.6-flare","prompt":"circle","output_format":"svg"`
	for _, suffix := range []string{`}`, `,"svg":{"model":"unpriced-author"}}`} {
		result := post(request + suffix)
		assert.Equal(t, http.StatusBadRequest, result.Code, result.Body.String())
		assert.Zero(t, calls.Load())
	}
	expensive := post(request + `,"svg":{"model":"expensive-author"}}`)
	assert.Equal(t, http.StatusForbidden, expensive.Code, expensive.Body.String())
	require.Zero(t, calls.Load())
	result := post(request + `,"svg":{"model":"gpt-5.4-mini"}}`)
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	assert.JSONEq(t, response, result.Body.String())
	plain := post(`{"model":"gpt-image-2.6-flare","prompt":"circle","output_format":"png"}`)
	require.Equal(t, http.StatusOK, plain.Code, plain.Body.String())
	var logs []model.Log
	require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Order("id").Find(&logs).Error)
	require.Len(t, logs, 2)
	assert.Equal(t, 13531, logs[0].Quota)
	assert.Equal(t, 12583, logs[1].Quota)
	assert.Equal(t, "gpt-5.4-mini", gjson.Get(logs[0].Other, "svg_author.billing_model").String())
	assert.EqualValues(t, 948, gjson.Get(logs[0].Other, "svg_author.quota").Int())
	assert.False(t, gjson.Get(logs[1].Other, "svg_author").Exists())
}

func setupRelayRouterTestDB(t *testing.T) {
	t.Helper()

	gin.SetMode(gin.TestMode)
	originalIsMasterNode := common.IsMasterNode
	originalRedisEnabled := common.RedisEnabled
	originalSQLitePath := common.SQLitePath
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalSQLDSN, hadSQLDSN := os.LookupEnv("SQL_DSN")

	common.IsMasterNode = false
	common.RedisEnabled = false
	common.SQLitePath = fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, os.Setenv("SQL_DSN", "local"))
	require.NoError(t, model.InitDB())
	model.LOG_DB = model.DB
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))

	t.Cleanup(func() {
		if sqlDB, err := model.DB.DB(); err == nil {
			_ = sqlDB.Close()
		}
		common.IsMasterNode = originalIsMasterNode
		common.RedisEnabled = originalRedisEnabled
		common.SQLitePath = originalSQLitePath
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		if hadSQLDSN {
			require.NoError(t, os.Setenv("SQL_DSN", originalSQLDSN))
		} else {
			require.NoError(t, os.Unsetenv("SQL_DSN"))
		}
	})
}
