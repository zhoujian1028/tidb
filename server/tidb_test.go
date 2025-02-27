// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//go:build !race
// +build !race

package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/domain"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser"
	tmysql "github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/session"
	"github.com/pingcap/tidb/store/mockstore"
	"github.com/pingcap/tidb/testkit"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/logutil"
	"github.com/pingcap/tidb/util/plancodec"
	"github.com/pingcap/tidb/util/topsql/reporter"
	mockTopSQLReporter "github.com/pingcap/tidb/util/topsql/reporter/mock"
	"github.com/pingcap/tidb/util/topsql/tracecpu"
	mockTopSQLTraceCPU "github.com/pingcap/tidb/util/topsql/tracecpu/mock"
	"github.com/stretchr/testify/require"
)

type tidbTestSuite struct {
	*testServerClient
	tidbdrv *TiDBDriver
	server  *Server
	domain  *domain.Domain
	store   kv.Storage
}

func createTidbTestSuite(t *testing.T) (*tidbTestSuite, func()) {
	ts := &tidbTestSuite{testServerClient: newTestServerClient()}

	// setup tidbTestSuite
	var err error
	ts.store, err = mockstore.NewMockStore()
	session.DisableStats4Test()
	require.NoError(t, err)
	ts.domain, err = session.BootstrapSession(ts.store)
	require.NoError(t, err)
	ts.tidbdrv = NewTiDBDriver(ts.store)
	cfg := newTestConfig()
	cfg.Port = ts.port
	cfg.Status.ReportStatus = true
	cfg.Status.StatusPort = ts.statusPort
	cfg.Performance.TCPKeepAlive = true
	err = logutil.InitLogger(cfg.Log.ToLogConfig())
	require.NoError(t, err)

	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	ts.port = getPortFromTCPAddr(server.listener.Addr())
	ts.statusPort = getPortFromTCPAddr(server.statusListener.Addr())
	ts.server = server
	go func() {
		err := ts.server.Run()
		require.NoError(t, err)
	}()
	ts.waitUntilServerOnline()

	cleanup := func() {
		if ts.domain != nil {
			ts.domain.Close()
		}
		if ts.server != nil {
			ts.server.Close()
		}
		if ts.store != nil {
			require.NoError(t, ts.store.Close())
		}
	}

	return ts, cleanup
}

type tidbTestTopSQLSuite struct {
	*tidbTestSuite
}

func createTidbTestTopSQLSuite(t *testing.T) (*tidbTestTopSQLSuite, func()) {
	base, cleanup := createTidbTestSuite(t)

	ts := &tidbTestTopSQLSuite{base}

	// Initialize global variable for top-sql test.
	db, err := sql.Open("mysql", ts.getDSN())
	require.NoError(t, err)
	defer func() {
		err := db.Close()
		require.NoError(t, err)
	}()

	dbt := testkit.NewDBTestKit(t, db)
	dbt.MustExec("set @@global.tidb_top_sql_precision_seconds=1;")
	dbt.MustExec("set @@global.tidb_top_sql_report_interval_seconds=2;")
	dbt.MustExec("set @@global.tidb_top_sql_max_statement_count=5;")

	tracecpu.GlobalSQLCPUProfiler.Run()

	return ts, cleanup
}

func TestRegression(t *testing.T) {
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()
	if regression {
		t.Parallel()
		ts.runTestRegression(t, nil, "Regression")
	}
}

func TestUint64(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestPrepareResultFieldType(t)
}

func TestSpecialType(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestSpecialType(t)
}

func TestPreparedString(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestPreparedString(t)
}

func TestPreparedTimestamp(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestPreparedTimestamp(t)
}

func TestConcurrentUpdate(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestConcurrentUpdate(t)
}

func TestErrorCode(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestErrorCode(t)
}

func TestAuth(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestAuth(t)
	ts.runTestIssue3682(t)
}

func TestIssues(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestIssue3662(t)
	ts.runTestIssue3680(t)
	ts.runTestIssue22646(t)
}

func TestDBNameEscape(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()
	ts.runTestDBNameEscape(t)
}

func TestResultFieldTableIsNull(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestResultFieldTableIsNull(t)
}

func TestStatusAPI(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestStatusAPI(t)
}

func TestStatusPort(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	cfg := newTestConfig()
	cfg.Port = 0
	cfg.Status.ReportStatus = true
	cfg.Status.StatusPort = ts.statusPort
	cfg.Performance.TCPKeepAlive = true

	server, err := NewServer(cfg, ts.tidbdrv)
	require.Error(t, err)
	require.Nil(t, server)
}

func TestStatusAPIWithTLS(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	caCert, caKey, err := generateCert(0, "TiDB CA 2", nil, nil, "/tmp/ca-key-2.pem", "/tmp/ca-cert-2.pem")
	require.NoError(t, err)
	_, _, err = generateCert(1, "tidb-server-2", caCert, caKey, "/tmp/server-key-2.pem", "/tmp/server-cert-2.pem")
	require.NoError(t, err)

	defer func() {
		os.Remove("/tmp/ca-key-2.pem")
		os.Remove("/tmp/ca-cert-2.pem")
		os.Remove("/tmp/server-key-2.pem")
		os.Remove("/tmp/server-cert-2.pem")
	}()

	cli := newTestServerClient()
	cli.statusScheme = "https"
	cfg := newTestConfig()
	cfg.Port = cli.port
	cfg.Status.StatusPort = cli.statusPort
	cfg.Security.ClusterSSLCA = "/tmp/ca-cert-2.pem"
	cfg.Security.ClusterSSLCert = "/tmp/server-cert-2.pem"
	cfg.Security.ClusterSSLKey = "/tmp/server-key-2.pem"
	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	cli.port = getPortFromTCPAddr(server.listener.Addr())
	cli.statusPort = getPortFromTCPAddr(server.statusListener.Addr())
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	time.Sleep(time.Millisecond * 100)

	// https connection should work.
	ts.runTestStatusAPI(t)

	// but plain http connection should fail.
	cli.statusScheme = "http"
	_, err = cli.fetchStatus("/status") // nolint: bodyclose
	require.Error(t, err)

	server.Close()
}

func TestStatusAPIWithTLSCNCheck(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	caPath := filepath.Join(os.TempDir(), "ca-cert-cn.pem")
	serverKeyPath := filepath.Join(os.TempDir(), "server-key-cn.pem")
	serverCertPath := filepath.Join(os.TempDir(), "server-cert-cn.pem")
	client1KeyPath := filepath.Join(os.TempDir(), "client-key-cn-check-a.pem")
	client1CertPath := filepath.Join(os.TempDir(), "client-cert-cn-check-a.pem")
	client2KeyPath := filepath.Join(os.TempDir(), "client-key-cn-check-b.pem")
	client2CertPath := filepath.Join(os.TempDir(), "client-cert-cn-check-b.pem")

	caCert, caKey, err := generateCert(0, "TiDB CA CN CHECK", nil, nil, filepath.Join(os.TempDir(), "ca-key-cn.pem"), caPath)
	require.NoError(t, err)
	_, _, err = generateCert(1, "tidb-server-cn-check", caCert, caKey, serverKeyPath, serverCertPath)
	require.NoError(t, err)
	_, _, err = generateCert(2, "tidb-client-cn-check-a", caCert, caKey, client1KeyPath, client1CertPath, func(c *x509.Certificate) {
		c.Subject.CommonName = "tidb-client-1"
	})
	require.NoError(t, err)
	_, _, err = generateCert(3, "tidb-client-cn-check-b", caCert, caKey, client2KeyPath, client2CertPath, func(c *x509.Certificate) {
		c.Subject.CommonName = "tidb-client-2"
	})
	require.NoError(t, err)

	cli := newTestServerClient()
	cli.statusScheme = "https"
	cfg := newTestConfig()
	cfg.Port = cli.port
	cfg.Status.StatusPort = cli.statusPort
	cfg.Security.ClusterSSLCA = caPath
	cfg.Security.ClusterSSLCert = serverCertPath
	cfg.Security.ClusterSSLKey = serverKeyPath
	cfg.Security.ClusterVerifyCN = []string{"tidb-client-2"}
	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)

	cli.port = getPortFromTCPAddr(server.listener.Addr())
	cli.statusPort = getPortFromTCPAddr(server.statusListener.Addr())
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	defer server.Close()
	time.Sleep(time.Millisecond * 100)

	hc := newTLSHttpClient(t, caPath,
		client1CertPath,
		client1KeyPath,
	)
	_, err = hc.Get(cli.statusURL("/status")) // nolint: bodyclose
	require.Error(t, err)

	hc = newTLSHttpClient(t, caPath,
		client2CertPath,
		client2KeyPath,
	)
	resp, err := hc.Get(cli.statusURL("/status"))
	require.NoError(t, err)
	require.Nil(t, resp.Body.Close())
}

func newTLSHttpClient(t *testing.T, caFile, certFile, keyFile string) *http.Client {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	require.NoError(t, err)
	caCert, err := os.ReadFile(caFile)
	require.NoError(t, err)
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		RootCAs:            caCertPool,
		InsecureSkipVerify: true,
	}
	tlsConfig.BuildNameToCertificate()
	return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
}

func TestMultiStatements(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runFailedTestMultiStatements(t)
	ts.runTestMultiStatements(t)
}

func TestSocketForwarding(t *testing.T) {
	t.Parallel()
	osTempDir := os.TempDir()
	tempDir, err := os.MkdirTemp(osTempDir, "tidb-test.*.socket")
	require.NoError(t, err)
	socketFile := tempDir + "/tidbtest.sock" // Unix Socket does not work on Windows, so '/' should be OK
	defer os.RemoveAll(tempDir)

	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	cli := newTestServerClient()
	cfg := newTestConfig()
	cfg.Socket = socketFile
	cfg.Port = cli.port
	os.Remove(cfg.Socket)
	cfg.Status.ReportStatus = false

	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	cli.port = getPortFromTCPAddr(server.listener.Addr())
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	time.Sleep(time.Millisecond * 100)
	defer server.Close()

	cli.runTestRegression(t, func(config *mysql.Config) {
		config.User = "root"
		config.Net = "unix"
		config.Addr = socketFile
		config.DBName = "test"
		config.Params = map[string]string{"sql_mode": "'STRICT_ALL_TABLES'"}
	}, "SocketRegression")
}

func TestSocket(t *testing.T) {
	t.Parallel()
	osTempDir := os.TempDir()
	tempDir, err := os.MkdirTemp(osTempDir, "tidb-test.*.socket")
	require.NoError(t, err)
	socketFile := tempDir + "/tidbtest.sock" // Unix Socket does not work on Windows, so '/' should be OK
	defer os.RemoveAll(tempDir)

	cfg := newTestConfig()
	cfg.Socket = socketFile
	cfg.Port = 0
	os.Remove(cfg.Socket)
	cfg.Host = ""
	cfg.Status.ReportStatus = false

	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	time.Sleep(time.Millisecond * 100)
	defer server.Close()

	confFunc := func(config *mysql.Config) {
		config.User = "root"
		config.Net = "unix"
		config.Addr = socketFile
		config.DBName = "test"
		config.Params = map[string]string{"sql_mode": "STRICT_ALL_TABLES"}
	}
	// a fake server client, config is override, just used to run tests
	cli := newTestServerClient()
	cli.waitUntilCustomServerCanConnect(confFunc)
	cli.runTestRegression(t, confFunc, "SocketRegression")
}

func TestSocketAndIp(t *testing.T) {
	t.Parallel()
	osTempDir := os.TempDir()
	tempDir, err := os.MkdirTemp(osTempDir, "tidb-test.*.socket")
	require.NoError(t, err)
	socketFile := tempDir + "/tidbtest.sock" // Unix Socket does not work on Windows, so '/' should be OK
	defer os.RemoveAll(tempDir)

	cli := newTestServerClient()
	cfg := newTestConfig()
	cfg.Socket = socketFile
	cfg.Port = cli.port
	cfg.Status.ReportStatus = false

	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	cli.port = getPortFromTCPAddr(server.listener.Addr())
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	cli.waitUntilServerCanConnect()
	defer server.Close()

	// Test with Socket connection + Setup user1@% for all host access
	cli.port = getPortFromTCPAddr(server.listener.Addr())
	defer func() {
		cli.runTests(t, func(config *mysql.Config) {
			config.User = "root"
		},
			func(dbt *testkit.DBTestKit) {
				dbt.MustExec("DROP USER IF EXISTS 'user1'@'%'")
				dbt.MustExec("DROP USER IF EXISTS 'user1'@'localhost'")
				dbt.MustExec("DROP USER IF EXISTS 'user1'@'127.0.0.1'")
			})
	}()
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "root"
		config.Net = "unix"
		config.Addr = socketFile
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "root@localhost")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
			dbt.MustQuery("CREATE USER user1@'%'")
			dbt.MustQuery("GRANT SELECT ON test.* TO user1@'%'")
		})
	// Test with Network interface connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			// NOTICE: this is not compatible with MySQL! (MySQL would report user1@localhost also for 127.0.0.1)
			cli.checkRows(t, rows, "user1@127.0.0.1")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'%'\nGRANT SELECT ON test.* TO 'user1'@'%'")
			rows = dbt.MustQuery("select host from information_schema.processlist where user = 'user1'")
			records := cli.Rows(t, rows)
			require.Contains(t, records[0], ":", "Missing :<port> in is.processlist")
		})
	// Test with unix domain socket file connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "user1@localhost")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'%'\nGRANT SELECT ON test.* TO 'user1'@'%'")
		})

	// Setup user1@127.0.0.1 for loop back network interface access
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "root"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			// NOTICE: this is not compatible with MySQL! (MySQL would report user1@localhost also for 127.0.0.1)
			cli.checkRows(t, rows, "root@127.0.0.1")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
			dbt.MustQuery("CREATE USER user1@127.0.0.1")
			dbt.MustQuery("GRANT SELECT,INSERT ON test.* TO user1@'127.0.0.1'")
		})
	// Test with Network interface connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			// NOTICE: this is not compatible with MySQL! (MySQL would report user1@localhost also for 127.0.0.1)
			cli.checkRows(t, rows, "user1@127.0.0.1")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'127.0.0.1'\nGRANT SELECT,INSERT ON test.* TO 'user1'@'127.0.0.1'")
		})
	// Test with unix domain socket file connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "user1@localhost")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'%'\nGRANT SELECT ON test.* TO 'user1'@'%'")
		})

	// Setup user1@localhost for socket (and if MySQL compatible; loop back network interface access)
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "root"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "root@localhost")
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
			dbt.MustExec("CREATE USER user1@localhost")
			dbt.MustExec("GRANT SELECT,INSERT,UPDATE,DELETE ON test.* TO user1@localhost")
		})
	// Test with Network interface connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			// NOTICE: this is not compatible with MySQL! (MySQL would report user1@localhost also for 127.0.0.1)
			cli.checkRows(t, rows, "user1@127.0.0.1")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'127.0.0.1'\nGRANT SELECT,INSERT ON test.* TO 'user1'@'127.0.0.1'")
			require.NoError(t, rows.Close())
		})
	// Test with unix domain socket file connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "user1@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'localhost'\nGRANT SELECT,INSERT,UPDATE,DELETE ON test.* TO 'user1'@'localhost'")
			require.NoError(t, rows.Close())
		})

}

// TestOnlySocket for server configuration without network interface for mysql clients
func TestOnlySocket(t *testing.T) {
	t.Parallel()
	osTempDir := os.TempDir()
	tempDir, err := os.MkdirTemp(osTempDir, "tidb-test.*.socket")
	require.NoError(t, err)
	socketFile := tempDir + "/tidbtest.sock" // Unix Socket does not work on Windows, so '/' should be OK
	defer os.RemoveAll(tempDir)

	cli := newTestServerClient()
	cfg := newTestConfig()
	cfg.Socket = socketFile
	cfg.Host = "" // No network interface listening for mysql traffic
	cfg.Status.ReportStatus = false

	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	time.Sleep(time.Millisecond * 100)
	defer server.Close()
	require.Nil(t, server.listener)
	require.NotNil(t, server.socket)

	// Test with Socket connection + Setup user1@% for all host access
	defer func() {
		cli.runTests(t, func(config *mysql.Config) {
			config.User = "root"
			config.Net = "unix"
			config.Addr = socketFile
		},
			func(dbt *testkit.DBTestKit) {
				dbt.MustExec("DROP USER IF EXISTS 'user1'@'%'")
				dbt.MustExec("DROP USER IF EXISTS 'user1'@'localhost'")
				dbt.MustExec("DROP USER IF EXISTS 'user1'@'127.0.0.1'")
			})
	}()
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "root"
		config.Net = "unix"
		config.Addr = socketFile
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "root@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
			require.NoError(t, rows.Close())
			dbt.MustExec("CREATE USER user1@'%'")
			dbt.MustExec("GRANT SELECT ON test.* TO user1@'%'")
		})
	// Test with Network interface connection with all hosts, should fail since server not configured
	db, err := sql.Open("mysql", cli.getDSN(func(config *mysql.Config) {
		config.User = "root"
		config.DBName = "test"
	}))
	require.NoErrorf(t, err, "Open failed")
	err = db.Ping()
	require.Errorf(t, err, "Connect succeeded when not configured!?!")
	db.Close()
	db, err = sql.Open("mysql", cli.getDSN(func(config *mysql.Config) {
		config.User = "user1"
		config.DBName = "test"
	}))
	require.NoErrorf(t, err, "Open failed")
	err = db.Ping()
	require.Errorf(t, err, "Connect succeeded when not configured!?!")
	db.Close()
	// Test with unix domain socket file connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "user1@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'%'\nGRANT SELECT ON test.* TO 'user1'@'%'")
			require.NoError(t, rows.Close())
		})

	// Setup user1@127.0.0.1 for loop back network interface access
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "root"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			// NOTICE: this is not compatible with MySQL! (MySQL would report user1@localhost also for 127.0.0.1)
			cli.checkRows(t, rows, "root@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
			require.NoError(t, rows.Close())
			dbt.MustExec("CREATE USER user1@127.0.0.1")
			dbt.MustExec("GRANT SELECT,INSERT ON test.* TO user1@'127.0.0.1'")
		})
	// Test with unix domain socket file connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "user1@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'%'\nGRANT SELECT ON test.* TO 'user1'@'%'")
			require.NoError(t, rows.Close())
		})

	// Setup user1@localhost for socket (and if MySQL compatible; loop back network interface access)
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "root"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "root@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
			require.NoError(t, rows.Close())
			dbt.MustExec("CREATE USER user1@localhost")
			dbt.MustExec("GRANT SELECT,INSERT,UPDATE,DELETE ON test.* TO user1@localhost")
		})
	// Test with unix domain socket file connection with all hosts
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "user1"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "user1@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'user1'@'localhost'\nGRANT SELECT,INSERT,UPDATE,DELETE ON test.* TO 'user1'@'localhost'")
			require.NoError(t, rows.Close())
		})

}

// generateCert generates a private key and a certificate in PEM format based on parameters.
// If parentCert and parentCertKey is specified, the new certificate will be signed by the parentCert.
// Otherwise, the new certificate will be self-signed and is a CA.
func generateCert(sn int, commonName string, parentCert *x509.Certificate, parentCertKey *rsa.PrivateKey, outKeyFile string, outCertFile string, opts ...func(c *x509.Certificate)) (*x509.Certificate, *rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 528)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	notBefore := time.Now().Add(-10 * time.Minute).UTC()
	notAfter := notBefore.Add(1 * time.Hour).UTC()

	template := x509.Certificate{
		SerialNumber:          big.NewInt(int64(sn)),
		Subject:               pkix.Name{CommonName: commonName, Names: []pkix.AttributeTypeAndValue{util.MockPkixAttribute(util.CommonName, commonName)}},
		DNSNames:              []string{commonName},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	for _, opt := range opts {
		opt(&template)
	}

	var parent *x509.Certificate
	var priv *rsa.PrivateKey

	if parentCert == nil || parentCertKey == nil {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
		parent = &template
		priv = privateKey
	} else {
		parent = parentCert
		priv = parentCertKey
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, parent, &privateKey.PublicKey, priv)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	cert, err := x509.ParseCertificate(derBytes)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	certOut, err := os.Create(outCertFile)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	err = certOut.Close()
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	keyOut, err := os.OpenFile(outKeyFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	err = pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	err = keyOut.Close()
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	return cert, privateKey, nil
}

// registerTLSConfig registers a mysql client TLS config.
// See https://godoc.org/github.com/go-sql-driver/mysql#RegisterTLSConfig for details.
func registerTLSConfig(configName string, caCertPath string, clientCertPath string, clientKeyPath string, serverName string, verifyServer bool) error {
	rootCertPool := x509.NewCertPool()
	data, err := os.ReadFile(caCertPath)
	if err != nil {
		return err
	}
	if ok := rootCertPool.AppendCertsFromPEM(data); !ok {
		return errors.New("Failed to append PEM")
	}
	clientCert := make([]tls.Certificate, 0, 1)
	certs, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		return err
	}
	clientCert = append(clientCert, certs)
	tlsConfig := &tls.Config{
		RootCAs:            rootCertPool,
		Certificates:       clientCert,
		ServerName:         serverName,
		InsecureSkipVerify: !verifyServer,
	}
	return mysql.RegisterTLSConfig(configName, tlsConfig)
}

func TestSystemTimeZone(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	tk := testkit.NewTestKit(t, ts.store)
	cfg := newTestConfig()
	cfg.Port, cfg.Status.StatusPort = 0, 0
	cfg.Status.ReportStatus = false
	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	defer server.Close()

	tz1 := tk.MustQuery("select variable_value from mysql.tidb where variable_name = 'system_tz'").Rows()
	tk.MustQuery("select @@system_time_zone").Check(tz1)
}

func TestClientWithCollation(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	ts.runTestClientWithCollation(t)
}

func TestCreateTableFlen(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	// issue #4540
	qctx, err := ts.tidbdrv.OpenCtx(uint64(0), 0, uint8(tmysql.DefaultCollationID), "test", nil)
	require.NoError(t, err)
	_, err = Execute(context.Background(), qctx, "use test;")
	require.NoError(t, err)

	ctx := context.Background()
	testSQL := "CREATE TABLE `t1` (" +
		"`a` char(36) NOT NULL," +
		"`b` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP," +
		"`c` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP," +
		"`d` varchar(50) DEFAULT ''," +
		"`e` char(36) NOT NULL DEFAULT ''," +
		"`f` char(36) NOT NULL DEFAULT ''," +
		"`g` char(1) NOT NULL DEFAULT 'N'," +
		"`h` varchar(100) NOT NULL," +
		"`i` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP," +
		"`j` varchar(10) DEFAULT ''," +
		"`k` varchar(10) DEFAULT ''," +
		"`l` varchar(20) DEFAULT ''," +
		"`m` varchar(20) DEFAULT ''," +
		"`n` varchar(30) DEFAULT ''," +
		"`o` varchar(100) DEFAULT ''," +
		"`p` varchar(50) DEFAULT ''," +
		"`q` varchar(50) DEFAULT ''," +
		"`r` varchar(100) DEFAULT ''," +
		"`s` varchar(20) DEFAULT ''," +
		"`t` varchar(50) DEFAULT ''," +
		"`u` varchar(100) DEFAULT ''," +
		"`v` varchar(50) DEFAULT ''," +
		"`w` varchar(300) NOT NULL," +
		"`x` varchar(250) DEFAULT ''," +
		"`y` decimal(20)," +
		"`z` decimal(20, 4)," +
		"PRIMARY KEY (`a`)" +
		") ENGINE=InnoDB DEFAULT CHARSET=utf8 COLLATE=utf8_bin"
	_, err = Execute(ctx, qctx, testSQL)
	require.NoError(t, err)
	rs, err := Execute(ctx, qctx, "show create table t1")
	require.NoError(t, err)
	req := rs.NewChunk(nil)
	err = rs.Next(ctx, req)
	require.NoError(t, err)
	cols := rs.Columns()
	require.NoError(t, err)
	require.Len(t, cols, 2)
	require.Equal(t, 5*tmysql.MaxBytesOfCharacter, int(cols[0].ColumnLength))
	require.Equal(t, len(req.GetRow(0).GetString(1))*tmysql.MaxBytesOfCharacter, int(cols[1].ColumnLength))

	// for issue#5246
	rs, err = Execute(ctx, qctx, "select y, z from t1")
	require.NoError(t, err)
	cols = rs.Columns()
	require.Len(t, cols, 2)
	require.Equal(t, 21, int(cols[0].ColumnLength))
	require.Equal(t, 22, int(cols[1].ColumnLength))
}

func Execute(ctx context.Context, qc *TiDBContext, sql string) (ResultSet, error) {
	stmts, err := qc.Parse(ctx, sql)
	if err != nil {
		return nil, err
	}
	if len(stmts) != 1 {
		panic("wrong input for Execute: " + sql)
	}
	return qc.ExecuteStmt(ctx, stmts[0])
}

func TestShowTablesFlen(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	qctx, err := ts.tidbdrv.OpenCtx(uint64(0), 0, uint8(tmysql.DefaultCollationID), "test", nil)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = Execute(ctx, qctx, "use test;")
	require.NoError(t, err)

	testSQL := "create table abcdefghijklmnopqrstuvwxyz (i int)"
	_, err = Execute(ctx, qctx, testSQL)
	require.NoError(t, err)
	rs, err := Execute(ctx, qctx, "show tables")
	require.NoError(t, err)
	req := rs.NewChunk(nil)
	err = rs.Next(ctx, req)
	require.NoError(t, err)
	cols := rs.Columns()
	require.NoError(t, err)
	require.Len(t, cols, 1)
	require.Equal(t, 26*tmysql.MaxBytesOfCharacter, int(cols[0].ColumnLength))
}

func checkColNames(t *testing.T, columns []*ColumnInfo, names ...string) {
	for i, name := range names {
		require.Equal(t, name, columns[i].Name)
		require.Equal(t, name, columns[i].OrgName)
	}
}

func TestFieldList(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	qctx, err := ts.tidbdrv.OpenCtx(uint64(0), 0, uint8(tmysql.DefaultCollationID), "test", nil)
	require.NoError(t, err)
	_, err = Execute(context.Background(), qctx, "use test;")
	require.NoError(t, err)

	ctx := context.Background()
	testSQL := `create table t (
		c_bit bit(10),
		c_int_d int,
		c_bigint_d bigint,
		c_float_d float,
		c_double_d double,
		c_decimal decimal(6, 3),
		c_datetime datetime(2),
		c_time time(3),
		c_date date,
		c_timestamp timestamp(4) DEFAULT CURRENT_TIMESTAMP(4),
		c_char char(20),
		c_varchar varchar(20),
		c_text_d text,
		c_binary binary(20),
		c_blob_d blob,
		c_set set('a', 'b', 'c'),
		c_enum enum('a', 'b', 'c'),
		c_json JSON,
		c_year year
	)`
	_, err = Execute(ctx, qctx, testSQL)
	require.NoError(t, err)
	colInfos, err := qctx.FieldList("t")
	require.NoError(t, err)
	require.Len(t, colInfos, 19)

	checkColNames(t, colInfos, "c_bit", "c_int_d", "c_bigint_d", "c_float_d",
		"c_double_d", "c_decimal", "c_datetime", "c_time", "c_date", "c_timestamp",
		"c_char", "c_varchar", "c_text_d", "c_binary", "c_blob_d", "c_set", "c_enum",
		"c_json", "c_year")

	for _, cols := range colInfos {
		require.Equal(t, "test", cols.Schema)
	}

	for _, cols := range colInfos {
		require.Equal(t, "t", cols.Table)
	}

	for i, col := range colInfos {
		switch i {
		case 10, 11, 12, 15, 16:
			// c_char char(20), c_varchar varchar(20), c_text_d text,
			// c_set set('a', 'b', 'c'), c_enum enum('a', 'b', 'c')
			require.Equalf(t, uint16(tmysql.CharsetNameToID(tmysql.DefaultCharset)), col.Charset, "index %d", i)
			continue
		}

		require.Equalf(t, uint16(tmysql.CharsetNameToID("binary")), col.Charset, "index %d", i)
	}

	// c_decimal decimal(6, 3)
	require.Equal(t, uint8(3), colInfos[5].Decimal)

	// for issue#10513
	tooLongColumnAsName := "COALESCE(0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0)"
	columnAsName := tooLongColumnAsName[:tmysql.MaxAliasIdentifierLen]

	rs, err := Execute(ctx, qctx, "select "+tooLongColumnAsName)
	require.NoError(t, err)
	cols := rs.Columns()
	require.Equal(t, tooLongColumnAsName, cols[0].OrgName)
	require.Equal(t, columnAsName, cols[0].Name)

	rs, err = Execute(ctx, qctx, "select c_bit as '"+tooLongColumnAsName+"' from t")
	require.NoError(t, err)
	cols = rs.Columns()
	require.Equal(t, "c_bit", cols[0].OrgName)
	require.Equal(t, columnAsName, cols[0].Name)
}

func TestClientErrors(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()
	ts.runTestInfoschemaClientErrors(t)
}

func TestInitConnect(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()
	ts.runTestInitConnect(t)
}

func TestSumAvg(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()
	ts.runTestSumAvg(t)
}

func TestNullFlag(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	qctx, err := ts.tidbdrv.OpenCtx(uint64(0), 0, uint8(tmysql.DefaultCollationID), "test", nil)
	require.NoError(t, err)

	ctx := context.Background()
	{
		// issue #9689
		rs, err := Execute(ctx, qctx, "select 1")
		require.NoError(t, err)
		cols := rs.Columns()
		require.Len(t, cols, 1)
		expectFlag := uint16(tmysql.NotNullFlag | tmysql.BinaryFlag)
		require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
	}

	{
		// issue #19025
		rs, err := Execute(ctx, qctx, "select convert('{}', JSON)")
		require.NoError(t, err)
		cols := rs.Columns()
		require.Len(t, cols, 1)
		expectFlag := uint16(tmysql.BinaryFlag)
		require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
	}

	{
		// issue #18488
		_, err := Execute(ctx, qctx, "use test")
		require.NoError(t, err)
		_, err = Execute(ctx, qctx, "CREATE TABLE `test` (`iD` bigint(20) NOT NULL, `INT_TEST` int(11) DEFAULT NULL);")
		require.NoError(t, err)
		rs, err := Execute(ctx, qctx, `SELECT id + int_test as res FROM test  GROUP BY res ORDER BY res;`)
		require.NoError(t, err)
		cols := rs.Columns()
		require.Len(t, cols, 1)
		expectFlag := uint16(tmysql.BinaryFlag)
		require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
	}
	{

		rs, err := Execute(ctx, qctx, "select if(1, null, 1) ;")
		require.NoError(t, err)
		cols := rs.Columns()
		require.Len(t, cols, 1)
		expectFlag := uint16(tmysql.BinaryFlag)
		require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
	}
	{
		rs, err := Execute(ctx, qctx, "select CASE 1 WHEN 2 THEN 1 END ;")
		require.NoError(t, err)
		cols := rs.Columns()
		require.Len(t, cols, 1)
		expectFlag := uint16(tmysql.BinaryFlag)
		require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
	}
	{
		rs, err := Execute(ctx, qctx, "select NULL;")
		require.NoError(t, err)
		cols := rs.Columns()
		require.Len(t, cols, 1)
		expectFlag := uint16(tmysql.BinaryFlag)
		require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
	}
}

func TestNO_DEFAULT_VALUEFlag(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	// issue #21465
	qctx, err := ts.tidbdrv.OpenCtx(uint64(0), 0, uint8(tmysql.DefaultCollationID), "test", nil)
	require.NoError(t, err)

	ctx := context.Background()
	_, err = Execute(ctx, qctx, "use test")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "drop table if exists t")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "create table t(c1 int key, c2 int);")
	require.NoError(t, err)
	rs, err := Execute(ctx, qctx, "select c1 from t;")
	require.NoError(t, err)
	cols := rs.Columns()
	require.Len(t, cols, 1)
	expectFlag := uint16(tmysql.NotNullFlag | tmysql.PriKeyFlag | tmysql.NoDefaultValueFlag)
	require.Equal(t, expectFlag, dumpFlag(cols[0].Type, cols[0].Flag))
}

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	cli := newTestServerClient()
	cfg := newTestConfig()
	cfg.GracefulWaitBeforeShutdown = 2 // wait before shutdown
	cfg.Port = 0
	cfg.Status.StatusPort = 0
	cfg.Status.ReportStatus = true
	cfg.Performance.TCPKeepAlive = true
	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	require.NotNil(t, server)
	cli.port = getPortFromTCPAddr(server.listener.Addr())
	cli.statusPort = getPortFromTCPAddr(server.statusListener.Addr())
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	time.Sleep(time.Millisecond * 100)

	resp, err := cli.fetchStatus("/status") // server is up
	require.NoError(t, err)
	require.Nil(t, resp.Body.Close())

	go server.Close()
	time.Sleep(time.Millisecond * 500)

	resp, _ = cli.fetchStatus("/status") // should return 5xx code
	require.Equal(t, 500, resp.StatusCode)
	require.Nil(t, resp.Body.Close())

	time.Sleep(time.Second * 2)

	// nolint: bodyclose
	_, err = cli.fetchStatus("/status") // status is gone
	require.Error(t, err)
	require.Regexp(t, "connect: connection refused$", err.Error())
}

func TestPessimisticInsertSelectForUpdate(t *testing.T) {
	t.Parallel()
	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	qctx, err := ts.tidbdrv.OpenCtx(uint64(0), 0, uint8(tmysql.DefaultCollationID), "test", nil)
	require.NoError(t, err)
	defer qctx.Close()
	ctx := context.Background()
	_, err = Execute(ctx, qctx, "use test;")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "drop table if exists t1, t2")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "create table t1 (id int)")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "create table t2 (id int)")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "insert into t1 select 1")
	require.NoError(t, err)
	_, err = Execute(ctx, qctx, "begin pessimistic")
	require.NoError(t, err)
	rs, err := Execute(ctx, qctx, "INSERT INTO t2 (id) select id from t1 where id = 1 for update")
	require.NoError(t, err)
	require.Nil(t, rs) // should be no delay
}

type collectorWrapper struct {
	reporter.TopSQLReporter
}

func TestTopSQLCPUProfile(t *testing.T) {
	ts, cleanup := createTidbTestTopSQLSuite(t)
	defer cleanup()

	db, err := sql.Open("mysql", ts.getDSN())
	require.NoError(t, err)
	defer func() {
		err := db.Close()
		require.NoError(t, err)
	}()

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/domain/skipLoadSysVarCacheLoop", `return(true)`))
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/util/topsql/mockHighLoadForEachSQL", `return(true)`))
	defer func() {
		err = failpoint.Disable("github.com/pingcap/tidb/domain/skipLoadSysVarCacheLoop")
		require.NoError(t, err)
		err = failpoint.Disable("github.com/pingcap/tidb/util/topsql/mockHighLoadForEachSQL")
		require.NoError(t, err)
	}()

	collector := mockTopSQLTraceCPU.NewTopSQLCollector()
	tracecpu.GlobalSQLCPUProfiler.SetCollector(&collectorWrapper{collector})

	dbt := testkit.NewDBTestKit(t, db)
	dbt.MustExec("drop database if exists topsql")
	dbt.MustExec("create database topsql")
	dbt.MustExec("use topsql;")
	dbt.MustExec("create table t (a int auto_increment, b int, unique index idx(a));")
	dbt.MustExec("create table t1 (a int auto_increment, b int, unique index idx(a));")
	dbt.MustExec("create table t2 (a int auto_increment, b int, unique index idx(a));")
	dbt.MustExec("set @@global.tidb_enable_top_sql='On';")
	config.UpdateGlobal(func(conf *config.Config) {
		conf.TopSQL.ReceiverAddress = "127.0.0.1:4001"
	})
	dbt.MustExec("set @@global.tidb_top_sql_precision_seconds=1;")
	dbt.MustExec("set @@global.tidb_txn_mode = 'pessimistic'")

	// Test case 1: DML query: insert/update/replace/delete/select
	cases1 := []struct {
		sql        string
		planRegexp string
		cancel     func()
	}{
		{sql: "insert into t () values (),(),(),(),(),(),();", planRegexp: ""},
		{sql: "insert into t (b) values (1),(1),(1),(1),(1),(1),(1),(1);", planRegexp: ""},
		{sql: "update t set b=a where b is null limit 1;", planRegexp: ".*Limit.*TableReader.*"},
		{sql: "delete from t where b = a limit 2;", planRegexp: ".*Limit.*TableReader.*"},
		{sql: "replace into t (b) values (1),(1),(1),(1),(1),(1),(1),(1);", planRegexp: ""},
		{sql: "select * from t use index(idx) where a<10;", planRegexp: ".*IndexLookUp.*"},
		{sql: "select * from t ignore index(idx) where a>1000000000;", planRegexp: ".*TableReader.*"},
		{sql: "select /*+ HASH_JOIN(t1, t2) */ * from t t1 join t t2 on t1.a=t2.a where t1.b is not null;", planRegexp: ".*HashJoin.*"},
		{sql: "select /*+ INL_HASH_JOIN(t1, t2) */ * from t t1 join t t2 on t2.a=t1.a where t1.b is not null;", planRegexp: ".*IndexHashJoin.*"},
		{sql: "select * from t where a=1;", planRegexp: ".*Point_Get.*"},
		{sql: "select * from t where a in (1,2,3,4)", planRegexp: ".*Batch_Point_Get.*"},
	}
	for i, ca := range cases1 {
		ctx, cancel := context.WithCancel(context.Background())
		cases1[i].cancel = cancel
		sqlStr := ca.sql
		go ts.loopExec(ctx, t, func(db *sql.DB) {
			dbt := testkit.NewDBTestKit(t, db)
			if strings.HasPrefix(sqlStr, "select") {
				rows := dbt.MustQuery(sqlStr)
				require.NoError(t, rows.Close())
			} else {
				// Ignore error here since the error may be write conflict.
				db.Exec(sqlStr)
			}
		})
	}

	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Second*20)
	defer cancel()
	checkFn := func(sql, planRegexp string) {
		require.NoError(t, timeoutCtx.Err())
		stats := collector.GetSQLStatsBySQLWithRetry(sql, len(planRegexp) > 0)
		// since 1 sql may has many plan, check `len(stats) > 0` instead of `len(stats) == 1`.
		require.Greaterf(t, len(stats), 0, "sql: %v", sql)

		for _, s := range stats {
			sqlStr := collector.GetSQL(s.SQLDigest)
			encodedPlan := collector.GetPlan(s.PlanDigest)
			// Normalize the user SQL before check.
			normalizedSQL := parser.Normalize(sql)
			require.Equalf(t, normalizedSQL, sqlStr, "sql: %v", sql)
			// decode plan before check.
			normalizedPlan, err := plancodec.DecodeNormalizedPlan(encodedPlan)
			require.NoError(t, err)
			// remove '\n' '\t' before do regexp match.
			normalizedPlan = strings.Replace(normalizedPlan, "\n", " ", -1)
			normalizedPlan = strings.Replace(normalizedPlan, "\t", " ", -1)
			require.Regexpf(t, planRegexp, normalizedPlan, "sql: %v", sql)
		}
	}
	// Wait the top sql collector to collect profile data.
	collector.WaitCollectCnt(1)
	// Check result of test case 1.
	for _, ca := range cases1 {
		checkFn(ca.sql, ca.planRegexp)
		ca.cancel()
	}

	// Test case 2: prepare/execute sql
	cases2 := []struct {
		prepare    string
		args       []interface{}
		planRegexp string
		cancel     func()
	}{
		{prepare: "insert into t1 (b) values (?);", args: []interface{}{1}, planRegexp: ""},
		{prepare: "replace into t1 (b) values (?);", args: []interface{}{1}, planRegexp: ""},
		{prepare: "update t1 set b=a where b is null limit ?;", args: []interface{}{1}, planRegexp: ".*Limit.*TableReader.*"},
		{prepare: "delete from t1 where b = a limit ?;", args: []interface{}{1}, planRegexp: ".*Limit.*TableReader.*"},
		{prepare: "replace into t1 (b) values (?);", args: []interface{}{1}, planRegexp: ""},
		{prepare: "select * from t1 use index(idx) where a<?;", args: []interface{}{10}, planRegexp: ".*IndexLookUp.*"},
		{prepare: "select * from t1 ignore index(idx) where a>?;", args: []interface{}{1000000000}, planRegexp: ".*TableReader.*"},
		{prepare: "select /*+ HASH_JOIN(t1, t2) */ * from t1 t1 join t1 t2 on t1.a=t2.a where t1.b is not null;", args: nil, planRegexp: ".*HashJoin.*"},
		{prepare: "select /*+ INL_HASH_JOIN(t1, t2) */ * from t1 t1 join t1 t2 on t2.a=t1.a where t1.b is not null;", args: nil, planRegexp: ".*IndexHashJoin.*"},
		{prepare: "select * from t1 where a=?;", args: []interface{}{1}, planRegexp: ".*Point_Get.*"},
		{prepare: "select * from t1 where a in (?,?,?,?)", args: []interface{}{1, 2, 3, 4}, planRegexp: ".*Batch_Point_Get.*"},
	}
	for i, ca := range cases2 {
		ctx, cancel := context.WithCancel(context.Background())
		cases2[i].cancel = cancel
		prepare, args := ca.prepare, ca.args
		var stmt *sql.Stmt
		go ts.loopExec(ctx, t, func(db *sql.DB) {
			if stmt == nil {
				stmt, err = db.Prepare(prepare)
				require.NoError(t, err)
			}
			if strings.HasPrefix(prepare, "select") {
				rows, err := stmt.Query(args...)
				require.NoError(t, err)
				require.NoError(t, rows.Close())
			} else {
				// Ignore error here since the error may be write conflict.
				_, err = stmt.Exec(args...)
				require.NoError(t, err)
			}
		})
	}
	// Wait the top sql collector to collect profile data.
	collector.WaitCollectCnt(1)
	// Check result of test case 2.
	for _, ca := range cases2 {
		checkFn(ca.prepare, ca.planRegexp)
		ca.cancel()
	}

	// Test case 3: prepare, execute stmt using @val...
	cases3 := []struct {
		prepare    string
		args       []interface{}
		planRegexp string
		cancel     func()
	}{
		{prepare: "insert into t2 (b) values (?);", args: []interface{}{1}, planRegexp: ""},
		{prepare: "update t2 set b=a where b is null limit ?;", args: []interface{}{1}, planRegexp: ".*Limit.*TableReader.*"},
		{prepare: "delete from t2 where b = a limit ?;", args: []interface{}{1}, planRegexp: ".*Limit.*TableReader.*"},
		{prepare: "replace into t2 (b) values (?);", args: []interface{}{1}, planRegexp: ""},
		{prepare: "select * from t2 use index(idx) where a<?;", args: []interface{}{10}, planRegexp: ".*IndexLookUp.*"},
		{prepare: "select * from t2 ignore index(idx) where a>?;", args: []interface{}{1000000000}, planRegexp: ".*TableReader.*"},
		{prepare: "select /*+ HASH_JOIN(t1, t2) */ * from t2 t1 join t2 t2 on t1.a=t2.a where t1.b is not null;", args: nil, planRegexp: ".*HashJoin.*"},
		{prepare: "select /*+ INL_HASH_JOIN(t1, t2) */ * from t2 t1 join t2 t2 on t2.a=t1.a where t1.b is not null;", args: nil, planRegexp: ".*IndexHashJoin.*"},
		{prepare: "select * from t2 where a=?;", args: []interface{}{1}, planRegexp: ".*Point_Get.*"},
		{prepare: "select * from t2 where a in (?,?,?,?)", args: []interface{}{1, 2, 3, 4}, planRegexp: ".*Batch_Point_Get.*"},
	}
	for i, ca := range cases3 {
		ctx, cancel := context.WithCancel(context.Background())
		cases3[i].cancel = cancel
		prepare, args := ca.prepare, ca.args
		doPrepare := true
		go ts.loopExec(ctx, t, func(db *sql.DB) {
			if doPrepare {
				doPrepare = false
				_, err := db.Exec(fmt.Sprintf("prepare stmt from '%v'", prepare))
				require.NoError(t, err)
			}
			sqlBuf := bytes.NewBuffer(nil)
			sqlBuf.WriteString("execute stmt ")
			for i := range args {
				_, err = db.Exec(fmt.Sprintf("set @%c=%v", 'a'+i, args[i]))
				require.NoError(t, err)
				if i == 0 {
					sqlBuf.WriteString("using ")
				} else {
					sqlBuf.WriteByte(',')
				}
				sqlBuf.WriteByte('@')
				sqlBuf.WriteByte('a' + byte(i))
			}
			if strings.HasPrefix(prepare, "select") {
				rows, err := db.Query(sqlBuf.String())
				require.NoErrorf(t, err, "%v", sqlBuf.String())
				require.NoError(t, rows.Close())
			} else {
				// Ignore error here since the error may be write conflict.
				_, err = db.Exec(sqlBuf.String())
				require.NoError(t, err)
			}
		})
	}

	// Wait the top sql collector to collect profile data.
	collector.WaitCollectCnt(1)
	// Check result of test case 3.
	for _, ca := range cases3 {
		checkFn(ca.prepare, ca.planRegexp)
		ca.cancel()
	}

	// Test case 4: transaction commit
	ctx4, cancel4 := context.WithCancel(context.Background())
	defer cancel4()
	go ts.loopExec(ctx4, t, func(db *sql.DB) {
		db.Exec("begin")
		db.Exec("insert into t () values (),(),(),(),(),(),(),(),(),(),(),(),(),(),(),(),(),(),(),(),(),()")
		db.Exec("commit")
	})
	// Check result of test case 4.
	checkFn("commit", "")
}

func TestTopSQLAgent(t *testing.T) {
	t.Skip("unstable, skip it and fix it before 20210702")

	ts, cleanup := createTidbTestTopSQLSuite(t)
	defer cleanup()
	db, err := sql.Open("mysql", ts.getDSN())
	require.NoError(t, err, "Error connecting")
	defer func() {
		err := db.Close()
		require.NoError(t, err)
	}()
	agentServer, err := mockTopSQLReporter.StartMockAgentServer()
	require.NoError(t, err)
	defer func() {
		agentServer.Stop()
	}()

	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/util/topsql/reporter/resetTimeoutForTest", `return(true)`))
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/domain/skipLoadSysVarCacheLoop", `return(true)`))
	require.NoError(t, failpoint.Enable("github.com/pingcap/tidb/util/topsql/mockHighLoadForEachSQL", `return(true)`))
	defer func() {
		err := failpoint.Disable("github.com/pingcap/tidb/util/topsql/reporter/resetTimeoutForTest")
		require.NoError(t, err)
		err = failpoint.Disable("github.com/pingcap/tidb/domain/skipLoadSysVarCacheLoop")
		require.NoError(t, err)
		err = failpoint.Disable("github.com/pingcap/tidb/util/topsql/mockHighLoadForEachSQL")
		require.NoError(t, err)
	}()

	dbt := testkit.NewDBTestKit(t, db)
	dbt.MustExec("drop database if exists topsql")
	dbt.MustExec("create database topsql")
	dbt.MustExec("use topsql;")
	for i := 0; i < 20; i++ {
		dbt.MustExec(fmt.Sprintf("create table t%v (a int auto_increment, b int, unique index idx(a));", i))
		for j := 0; j < 100; j++ {
			dbt.MustExec(fmt.Sprintf("insert into t%v (b) values (%v);", i, j))
		}
	}
	setTopSQLReceiverAddress := func(addr string) {
		config.UpdateGlobal(func(conf *config.Config) {
			conf.TopSQL.ReceiverAddress = addr
		})
	}
	dbt.MustExec("set @@global.tidb_enable_top_sql='On';")
	setTopSQLReceiverAddress("")
	dbt.MustExec("set @@global.tidb_top_sql_precision_seconds=1;")
	dbt.MustExec("set @@global.tidb_top_sql_report_interval_seconds=2;")
	dbt.MustExec("set @@global.tidb_top_sql_max_statement_count=5;")

	r := reporter.NewRemoteTopSQLReporter(reporter.NewGRPCReportClient(plancodec.DecodeNormalizedPlan))
	tracecpu.GlobalSQLCPUProfiler.SetCollector(&collectorWrapper{r})

	// TODO: change to ensure that the right sql statements are reported, not just counts
	checkFn := func(n int) {
		records := agentServer.GetLatestRecords()
		require.Len(t, records, n)
		for _, r := range records {
			sqlMeta, exist := agentServer.GetSQLMetaByDigestBlocking(r.SqlDigest, time.Second)
			require.True(t, exist)
			require.Regexp(t, "^select.*from.*join", sqlMeta.NormalizedSql)
			if len(r.PlanDigest) == 0 {
				continue
			}
			plan, exist := agentServer.GetPlanMetaByDigestBlocking(r.PlanDigest, time.Second)
			require.True(t, exist)
			plan = strings.Replace(plan, "\n", " ", -1)
			plan = strings.Replace(plan, "\t", " ", -1)
			require.Regexp(t, "Join.*Select", plan)
		}
	}
	runWorkload := func(start, end int) context.CancelFunc {
		ctx, cancel := context.WithCancel(context.Background())
		for i := start; i < end; i++ {
			query := fmt.Sprintf("select /*+ HASH_JOIN(ta, tb) */ * from t%[1]v ta join t%[1]v tb on ta.a=tb.a where ta.b is not null;", i)
			go ts.loopExec(ctx, t, func(db *sql.DB) {
				dbt := testkit.NewDBTestKit(t, db)
				rows := dbt.MustQuery(query)
				require.NoError(t, rows.Close())
			})
		}
		return cancel
	}

	// case 1: dynamically change agent endpoint
	cancel := runWorkload(0, 10)
	// Test with null agent address, the agent server can't receive any record.
	setTopSQLReceiverAddress("")
	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(0)
	// Test after set agent address and the evict take effect.
	dbt.MustExec("set @@global.tidb_top_sql_max_statement_count=5;")
	setTopSQLReceiverAddress(agentServer.Address())
	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(5)
	// Test with wrong agent address, the agent server can't receive any record.
	dbt.MustExec("set @@global.tidb_top_sql_max_statement_count=8;")
	setTopSQLReceiverAddress("127.0.0.1:65530")

	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(0)
	// Test after set agent address and the evict take effect.
	setTopSQLReceiverAddress(agentServer.Address())
	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(8)
	cancel() // cancel case 1

	// case 2: agent hangs for a while
	cancel2 := runWorkload(0, 10)
	// empty agent address, should not collect records
	dbt.MustExec("set @@global.tidb_top_sql_max_statement_count=5;")
	setTopSQLReceiverAddress("")
	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(0)
	// set correct address, should collect records
	setTopSQLReceiverAddress(agentServer.Address())
	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(5)
	// agent server hangs for a while
	agentServer.HangFromNow(time.Second * 6)
	// run another set of SQL queries
	cancel2()

	cancel3 := runWorkload(11, 20)
	agentServer.WaitCollectCnt(1, time.Second*8)
	checkFn(5)
	cancel3()

	// case 3: agent restart
	cancel4 := runWorkload(0, 10)
	// empty agent address, should not collect records
	setTopSQLReceiverAddress("")
	agentServer.WaitCollectCnt(1, time.Second*4)
	checkFn(0)
	// set correct address, should collect records
	setTopSQLReceiverAddress(agentServer.Address())
	agentServer.WaitCollectCnt(1, time.Second*8)
	checkFn(5)
	// run another set of SQL queries
	cancel4()

	cancel5 := runWorkload(11, 20)
	// agent server shutdown
	agentServer.Stop()
	// agent server restart
	agentServer, err = mockTopSQLReporter.StartMockAgentServer()
	require.NoError(t, err)
	setTopSQLReceiverAddress(agentServer.Address())
	// check result
	agentServer.WaitCollectCnt(2, time.Second*8)
	checkFn(5)
	cancel5()
}

func (ts *tidbTestTopSQLSuite) loopExec(ctx context.Context, t *testing.T, fn func(db *sql.DB)) {
	db, err := sql.Open("mysql", ts.getDSN())
	require.NoError(t, err, "Error connecting")
	defer func() {
		err := db.Close()
		require.NoError(t, err)
	}()
	dbt := testkit.NewDBTestKit(t, db)
	dbt.MustExec("use topsql;")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		fn(db)
	}
}

func TestLocalhostClientMapping(t *testing.T) {
	t.Parallel()
	osTempDir := os.TempDir()
	tempDir, err := os.MkdirTemp(osTempDir, "tidb-test.*.socket")
	require.NoError(t, err)
	socketFile := tempDir + "/tidbtest.sock" // Unix Socket does not work on Windows, so '/' should be OK
	defer os.RemoveAll(tempDir)

	cli := newTestServerClient()
	cfg := newTestConfig()
	cfg.Socket = socketFile
	cfg.Port = cli.port
	cfg.Status.ReportStatus = false

	ts, cleanup := createTidbTestSuite(t)
	defer cleanup()

	server, err := NewServer(cfg, ts.tidbdrv)
	require.NoError(t, err)
	cli.port = getPortFromTCPAddr(server.listener.Addr())
	go func() {
		err := server.Run()
		require.NoError(t, err)
	}()
	defer server.Close()
	cli.waitUntilServerCanConnect()

	cli.port = getPortFromTCPAddr(server.listener.Addr())
	// Create a db connection for root
	db, err := sql.Open("mysql", cli.getDSN(func(config *mysql.Config) {
		config.User = "root"
		config.Net = "unix"
		config.DBName = "test"
		config.Addr = socketFile
	}))
	require.NoErrorf(t, err, "Open failed")
	err = db.Ping()
	require.NoErrorf(t, err, "Ping failed")
	defer db.Close()
	dbt := testkit.NewDBTestKit(t, db)
	rows := dbt.MustQuery("select user()")
	cli.checkRows(t, rows, "root@localhost")
	require.NoError(t, rows.Close())
	rows = dbt.MustQuery("show grants")
	cli.checkRows(t, rows, "GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' WITH GRANT OPTION")
	require.NoError(t, rows.Close())

	dbt.MustExec("CREATE USER 'localhostuser'@'localhost'")
	dbt.MustExec("CREATE USER 'localhostuser'@'%'")
	defer func() {
		dbt.MustExec("DROP USER IF EXISTS 'localhostuser'@'%'")
		dbt.MustExec("DROP USER IF EXISTS 'localhostuser'@'localhost'")
		dbt.MustExec("DROP USER IF EXISTS 'localhostuser'@'127.0.0.1'")
	}()

	dbt.MustExec("GRANT SELECT ON test.* TO 'localhostuser'@'%'")
	dbt.MustExec("GRANT SELECT,UPDATE ON test.* TO 'localhostuser'@'localhost'")

	// Test with loopback interface - Should get access to localhostuser@localhost!
	cli.runTests(t, func(config *mysql.Config) {
		config.User = "localhostuser"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			// NOTICE: this is not compatible with MySQL! (MySQL would report localhostuser@localhost also for 127.0.0.1)
			cli.checkRows(t, rows, "localhostuser@127.0.0.1")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'localhostuser'@'localhost'\nGRANT SELECT,UPDATE ON test.* TO 'localhostuser'@'localhost'")
			require.NoError(t, rows.Close())
		})

	dbt.MustExec("DROP USER IF EXISTS 'localhostuser'@'localhost'")
	dbt.MustExec("CREATE USER 'localhostuser'@'127.0.0.1'")
	dbt.MustExec("GRANT SELECT,UPDATE ON test.* TO 'localhostuser'@'127.0.0.1'")
	// Test with unix domain socket file connection - Should get access to '%'
	cli.runTests(t, func(config *mysql.Config) {
		config.Net = "unix"
		config.Addr = socketFile
		config.User = "localhostuser"
		config.DBName = "test"
	},
		func(dbt *testkit.DBTestKit) {
			rows := dbt.MustQuery("select user()")
			cli.checkRows(t, rows, "localhostuser@localhost")
			require.NoError(t, rows.Close())
			rows = dbt.MustQuery("show grants")
			cli.checkRows(t, rows, "GRANT USAGE ON *.* TO 'localhostuser'@'%'\nGRANT SELECT ON test.* TO 'localhostuser'@'%'")
			require.NoError(t, rows.Close())
		})

	// Test if only localhost exists
	dbt.MustExec("DROP USER 'localhostuser'@'%'")
	dbSocket, err := sql.Open("mysql", cli.getDSN(func(config *mysql.Config) {
		config.User = "localhostuser"
		config.Net = "unix"
		config.DBName = "test"
		config.Addr = socketFile
	}))
	require.NoErrorf(t, err, "Open failed")
	defer dbSocket.Close()
	err = dbSocket.Ping()
	require.Errorf(t, err, "Connection successful without matching host for unix domain socket!")
}
