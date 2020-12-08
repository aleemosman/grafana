package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch/cloudwatchiface"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs/cloudwatchlogsiface"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/tsdb"
	"github.com/grafana/grafana/pkg/tsdb/cloudwatch"

	cwapi "github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/infra/fs"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/ini.v1"
)

func TestQueryCloudWatchMetrics(t *testing.T) {
	grafDir, cfgPath := createGrafDir(t)
	sqlStore := setUpDatabase(t, grafDir)
	addr := startGrafana(t, grafDir, cfgPath, sqlStore)

	origNewCWClient := cloudwatch.NewCWClient
	t.Cleanup(func() {
		cloudwatch.NewCWClient = origNewCWClient
	})
	var client cloudwatch.FakeCWClient
	cloudwatch.NewCWClient = func(sess *session.Session) cloudwatchiface.CloudWatchAPI {
		return client
	}

	t.Run("Custom metrics", func(t *testing.T) {
		client = cloudwatch.FakeCWClient{
			Metrics: []*cwapi.Metric{
				{
					MetricName: aws.String("Test_MetricName"),
					Dimensions: []*cwapi.Dimension{
						{
							Name: aws.String("Test_DimensionName"),
						},
					},
				},
			},
		}

		req := dtos.MetricRequest{
			Queries: []*simplejson.Json{
				simplejson.NewFromAny(map[string]interface{}{
					"type":         "metricFindQuery",
					"subtype":      "metrics",
					"region":       "us-east-1",
					"namespace":    "custom",
					"datasourceId": 1,
				}),
			},
		}
		tr := makeCWRequest(t, req, addr)

		assert.Equal(t, tsdb.Response{
			Results: map[string]*tsdb.QueryResult{
				"A": {
					RefId: "A",
					Meta: simplejson.NewFromAny(map[string]interface{}{
						"rowCount": float64(1),
					}),
					Tables: []*tsdb.Table{
						{
							Columns: []tsdb.TableColumn{
								{
									Text: "text",
								},
								{
									Text: "value",
								},
							},
							Rows: []tsdb.RowValues{
								{
									"Test_MetricName",
									"Test_MetricName",
								},
							},
						},
					},
				},
			},
		}, tr)
	})

	/*
		fmt.Printf("Test sleeping\n")
		time.Sleep(5 * time.Second)
		fmt.Printf("Finished test sleep\n")
	*/
}

func TestQueryCloudWatchLogs(t *testing.T) {
	grafDir, cfgPath := createGrafDir(t)
	sqlStore := setUpDatabase(t, grafDir)
	addr := startGrafana(t, grafDir, cfgPath, sqlStore)

	origNewCWLogsClient := cloudwatch.NewCWLogsClient
	t.Cleanup(func() {
		cloudwatch.NewCWLogsClient = origNewCWLogsClient
	})

	var client cloudwatch.FakeCWLogsClient
	cloudwatch.NewCWLogsClient = func(sess *session.Session) cloudwatchlogsiface.CloudWatchLogsAPI {
		return client
	}

	t.Run("Describe log groups", func(t *testing.T) {
		client = cloudwatch.FakeCWLogsClient{}

		req := dtos.MetricRequest{
			Queries: []*simplejson.Json{
				simplejson.NewFromAny(map[string]interface{}{
					"type":         "logAction",
					"subtype":      "DescribeLogGroups",
					"region":       "us-east-1",
					"datasourceId": 1,
				}),
			},
		}
		tr := makeCWRequest(t, req, addr)

		dataFrames := tsdb.NewDecodedDataFrames(data.Frames{
			&data.Frame{
				Name: "logGroups",
				Fields: []*data.Field{
					data.NewField("logGroupName", nil, []*string{}),
				},
				Meta: &data.FrameMeta{
					PreferredVisualization: "logs",
				},
			},
		})
		// Have to call this so that dataFrames.encoded is non-nil, for the comparison
		// In the future we should use gocmp instead and ignore this field
		_, err := dataFrames.Encoded()
		require.NoError(t, err)
		assert.Equal(t, tsdb.Response{
			Results: map[string]*tsdb.QueryResult{
				"A": {
					RefId:      "A",
					Dataframes: dataFrames,
				},
			},
		}, tr)
	})
}

func makeCWRequest(t *testing.T, req dtos.MetricRequest, addr string) tsdb.Response {
	t.Helper()

	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	err := enc.Encode(&req)
	require.NoError(t, err)
	resp, err := http.Post(fmt.Sprintf("http://%s/api/ds/query", addr), "application/json", &buf)
	require.NoError(t, err)
	require.NotNil(t, resp)
	t.Cleanup(func() { resp.Body.Close() })

	buf = bytes.Buffer{}
	_, err = io.Copy(&buf, resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode)

	var tr tsdb.Response
	err = json.Unmarshal(buf.Bytes(), &tr)
	require.NoError(t, err)

	return tr
}

func createGrafDir(t *testing.T) (string, string) {
	t.Helper()

	tmpDir, err := ioutil.TempDir("", "")
	require.NoError(t, err)
	t.Cleanup(func() {
		os.RemoveAll(tmpDir)
	})

	rootDir := filepath.Join("..", "..")

	cfgDir := filepath.Join(tmpDir, "conf")
	err = os.MkdirAll(cfgDir, 0755)
	require.NoError(t, err)
	dataDir := filepath.Join(tmpDir, "data")
	err = os.MkdirAll(dataDir, 0755)
	require.NoError(t, err)
	logsDir := filepath.Join(tmpDir, "logs")
	pluginsDir := filepath.Join(tmpDir, "plugins")
	publicDir := filepath.Join(tmpDir, "public")
	err = os.MkdirAll(publicDir, 0755)
	require.NoError(t, err)
	emailsDir := filepath.Join(publicDir, "emails")
	err = fs.CopyRecursive(filepath.Join(rootDir, "public", "emails"), emailsDir)
	require.NoError(t, err)
	provDir := filepath.Join(cfgDir, "provisioning")
	provDSDir := filepath.Join(provDir, "datasources")
	err = os.MkdirAll(provDSDir, 0755)
	require.NoError(t, err)
	provNotifiersDir := filepath.Join(provDir, "notifiers")
	err = os.MkdirAll(provNotifiersDir, 0755)
	require.NoError(t, err)
	provPluginsDir := filepath.Join(provDir, "plugins")
	err = os.MkdirAll(provPluginsDir, 0755)
	require.NoError(t, err)
	provDashboardsDir := filepath.Join(provDir, "dashboards")
	err = os.MkdirAll(provDashboardsDir, 0755)
	require.NoError(t, err)

	cfg := ini.Empty()
	dfltSect := cfg.Section("")
	_, err = dfltSect.NewKey("app_mode", "development")
	require.NoError(t, err)

	pathsSect, err := cfg.NewSection("paths")
	require.NoError(t, err)
	_, err = pathsSect.NewKey("data", dataDir)
	require.NoError(t, err)
	_, err = pathsSect.NewKey("logs", logsDir)
	require.NoError(t, err)
	_, err = pathsSect.NewKey("plugins", pluginsDir)
	require.NoError(t, err)

	logSect, err := cfg.NewSection("log")
	require.NoError(t, err)
	_, err = logSect.NewKey("level", "debug")
	require.NoError(t, err)

	serverSect, err := cfg.NewSection("server")
	require.NoError(t, err)
	_, err = serverSect.NewKey("port", "0")
	require.NoError(t, err)

	anonSect, err := cfg.NewSection("auth.anonymous")
	require.NoError(t, err)
	_, err = anonSect.NewKey("enabled", "true")
	require.NoError(t, err)

	cfgPath := filepath.Join(cfgDir, "test.ini")
	err = cfg.SaveTo(cfgPath)
	require.NoError(t, err)

	err = fs.CopyFile(filepath.Join(rootDir, "conf", "defaults.ini"), filepath.Join(cfgDir, "defaults.ini"))
	require.NoError(t, err)

	return tmpDir, cfgPath
}

func startGrafana(t *testing.T, grafDir, cfgPath string, sqlStore *sqlstore.SQLStore) string {
	t.Helper()

	origSQLStore := registry.GetService(sqlstore.ServiceName)
	t.Cleanup(func() {
		registry.Register(origSQLStore)
	})

	registry.Register(&registry.Descriptor{
		Name:         sqlstore.ServiceName,
		Instance:     sqlStore,
		InitPriority: sqlstore.InitPriority,
	})

	t.Logf("Registered SQL store %p", sqlStore)

	/*
		time.Sleep(10 * time.Second)
		org, err = server.HTTPServer.SQLStore.GetOrgByName("Main Org.")
		if err != nil {
			panic(fmt.Sprintf("Couldn't get main org from %p", server.HTTPServer.SQLStore))
		}
		fmt.Printf("Could get the org after sleeping\n")
	*/
	fmt.Printf("\nCreating Grafana server\n\n")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server, err := New(Config{
		ConfigFile: cfgPath,
		HomePath:   grafDir,
		Listener:   listener,
	})
	require.NoError(t, err)

	go func() {
		// When the server runs, it will also build and initialize the service graph
		fmt.Printf("\nStarting Grafana\n\n")
		if err := server.Run(); err != nil {
			t.Log("Server exited uncleanly", "error", err)
		}
	}()
	t.Cleanup(func() {
		server.Shutdown("")
	})

	// Wait for Grafana to be ready
	fmt.Printf("Waiting for G\n")
	addr := listener.Addr().String()
	resp, err := http.Get(fmt.Sprintf("http://%s/api/health", addr))
	require.NoError(t, err)
	require.NotNil(t, resp)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	fmt.Printf("G is ready\n")
	t.Logf("Grafana is listening on %s", addr)

	return addr
}

func setUpDatabase(t *testing.T, grafDir string) *sqlstore.SQLStore {
	t.Helper()

	sqlStore := sqlstore.InitTestDB(t)
	t.Logf("Created SQLStore: %+v", sqlStore)

	err := sqlStore.WithDbSession(context.Background(), func(sess *sqlstore.DBSession) error {
		_, err := sess.Insert(&models.DataSource{
			Id:      1,
			OrgId:   1,
			Name:    "Test",
			Type:    "cloudwatch",
			Created: time.Now(),
			Updated: time.Now(),
		})
		return err
	})
	require.NoError(t, err)

	return sqlStore
}
