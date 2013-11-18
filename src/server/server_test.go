package server

import (
	"bytes"
	"common"
	"configuration"
	"datastore"
	"encoding/json"
	"fmt"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"net"
	"net/http"
	"net/url"
	"os"
	"parser"
	"protocol"
	"runtime"
	"testing"
	"time"
)

// Hook up gocheck into the gotest runner.
func Test(t *testing.T) {
	TestingT(t)
}

type ServerSuite struct {
	servers []*Server
}

var _ = Suite(&ServerSuite{})

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU() * 2)
}

func (self *ServerSuite) SetUpSuite(c *C) {
	self.servers = startCluster(3, c)
	time.Sleep(time.Second * 4)
	time.Sleep(time.Second)
	err := self.servers[0].RaftServer.CreateDatabase("test_rep", uint8(2))
	c.Assert(err, IsNil)
	time.Sleep(time.Millisecond * 10)
	_, err = self.postToServer(self.servers[0], "/db/test_rep/users?u=root&p=root", `{"username": "paul", "password": "pass"}`, c)
	c.Assert(err, IsNil)
}

func (self *ServerSuite) TearDownSuite(c *C) {
	for _, s := range self.servers {
		s.Stop()
		os.RemoveAll(s.Config.DataDir)
		os.RemoveAll(s.Config.RaftDir)
	}
}

func getAvailablePorts(count int, c *C) []int {
	listeners := make([]net.Listener, count, count)
	ports := make([]int, count, count)
	for i, _ := range listeners {
		l, err := net.Listen("tcp4", ":0")
		c.Assert(err, IsNil)
		port := l.Addr().(*net.TCPAddr).Port
		ports[i] = port
		listeners[i] = l
	}
	for _, l := range listeners {
		l.Close()
	}
	return ports
}

func getDir(prefix string, c *C) string {
	path, err := ioutil.TempDir(os.TempDir(), prefix)
	c.Assert(err, IsNil)
	return path
}

func startCluster(count int, c *C) []*Server {
	ports := getAvailablePorts(count*4, c)
	seedServerPort := ports[0]
	servers := make([]*Server, count, count)
	for i, _ := range servers {
		var seedServers []string
		if i == 0 {
			seedServers = []string{}
		} else {
			seedServers = []string{fmt.Sprintf("http://localhost:%d", seedServerPort)}
		}

		portOffset := i * 4
		config := &configuration.Configuration{
			RaftServerPort: ports[portOffset],
			AdminHttpPort:  ports[portOffset+1],
			ApiHttpPort:    ports[portOffset+2],
			ProtobufPort:   ports[portOffset+3],
			AdminAssetsDir: "./",
			DataDir:        getDir("influxdb_db", c),
			RaftDir:        getDir("influxdb_raft", c),
			SeedServers:    seedServers,
			Hostname:       "localhost",
		}
		server, err := NewServer(config)
		if err != nil {
			c.Error(err)
		}
		go func() {
			err := server.ListenAndServe()
			if err != nil {
				c.Error(err)
			}
		}()
		time.Sleep(time.Millisecond * 50)
		servers[i] = server
	}
	return servers
}

func (self *ServerSuite) postToServer(server *Server, url, data string, c *C) (*http.Response, error) {
	fullUrl := fmt.Sprintf("http://localhost:%d%s", server.Config.ApiHttpPort, url)
	resp, err := http.Post(fullUrl, "application/json", bytes.NewBufferString(data))
	c.Assert(err, IsNil)
	return resp, err
}

func executeQuery(user common.User, database, query string, db datastore.Datastore, c *C) []*protocol.Series {
	q, errQ := parser.ParseQuery(query)
	c.Assert(errQ, IsNil)
	resultSeries := []*protocol.Series{}
	yield := func(series *protocol.Series) error {
		// ignore time series which have no data, this includes
		// end of series indicator
		if len(series.Points) > 0 {
			resultSeries = append(resultSeries, series)
		}
		return nil
	}
	err := db.ExecuteQuery(user, database, q, yield)
	c.Assert(err, IsNil)
	return resultSeries
}

func (self *ServerSuite) TestDataReplication(c *C) {
	servers := self.servers

	data := `
  [{
    "points": [
        ["val1", 2]
    ],
    "name": "foo",
    "columns": ["val_1", "val_2"]
  }]`
	resp, _ := self.postToServer(servers[0], "/db/test_rep/series?u=paul&p=pass", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	time.Sleep(time.Millisecond * 10)

	countWithPoint := 0
	user := &MockUser{}
	for _, server := range servers {
		results := executeQuery(user, "test_rep", "select * from foo;", server.Db, c)
		pointCount := 0
		for _, series := range results {
			if *series.Name == "foo" {
				if len(series.Points) > 0 {
					pointCount += 1
				}
			} else {
				c.Error(fmt.Sprintf("Got a series in the query we didn't expect: %s", *series.Name))
			}
		}
		if pointCount > 1 {
			c.Error("Got too many points for the series from one db")
		} else if pointCount > 0 {
			countWithPoint += 1
		}
	}
	c.Assert(countWithPoint, Equals, 2)
}

func (self *ServerSuite) TestCrossClusterQueries(c *C) {
	data := `[{
		"name": "cluster_query",
		"columns": ["val1"],
		"points": [[1], [2], [3], [4]]
		}]`
	resp, _ := self.postToServer(self.servers[0], "/db/test_rep/series?u=paul&p=pass", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)

	time.Sleep(time.Second)
	data = `[{
		"name": "cluster_query",
		"columns": ["val1"],
		"points": [[5], [6], [7]]
		}]`
	resp, _ = self.postToServer(self.servers[0], "/db/test_rep/series?u=paul&p=pass", data, c)
	c.Assert(resp.StatusCode, Equals, http.StatusOK)
	time.Sleep(time.Millisecond * 100)

	for _, s := range self.servers {
		query := "select count(val1) from cluster_query;"
		encodedQuery := url.QueryEscape(query)
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/db/test_rep/series?u=paul&p=pass&q=%s", s.Config.ApiHttpPort, encodedQuery))
		c.Assert(err, IsNil)
		body, err := ioutil.ReadAll(resp.Body)
		c.Assert(err, IsNil)
		c.Assert(resp.StatusCode, Equals, http.StatusOK)
		resp.Body.Close()
		var results []map[string]interface{}
		err = json.Unmarshal(body, &results)
		c.Assert(err, IsNil)
		fmt.Println("RESULT: ", string(body))
		c.Assert(results, HasLen, 1)
		point := results[0]["points"].([]interface{})[0].([]interface{})
		val := point[len(point)-1].(float64)
		c.Assert(val, Equals, float64(7))
	}
}
