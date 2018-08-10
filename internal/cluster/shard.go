package cluster

import (
	"strings"

	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
	"github.com/percona/mongodb-backup/mdbstructs"
)

const (
	configDB = "config"
)

type Shard struct {
	shard   *mdbstructs.Shard
	replset *Replset
}

// parseShardURI takes in a MongoDB shard URI (in
// '<replica-set>/<host1>:<port1>,<host2>:<port2>,...'
// format) and returns a replica set and slice of hosts.
func parseShardURI(uri string) (string, []string) {
	s := strings.Split(uri, "/")
	if len(s) == 2 {
		return s[0], strings.Split(s[1], ",")
	}
	return "", []string{}
}

func NewShard(config *Config, shard *mdbstructs.Shard) (*Shard, error) {
	replset, addrs := parseShardURI(shard.Host)
	rs, err := NewReplset(config, replset, addrs)
	if err != nil {
		return nil, err
	}
	return &Shard{
		shard:   shard,
		replset: rs,
	}, nil
}

// Return shards within a sharded cluster using the MongoDB 'listShards'
// server command. This command will only succeed on a mongos, use
// .GetConfigsvrShards() to get shards from a config server.
//
// https://docs.mongodb.com/manual/reference/command/listShards/
//
func GetListShards(session *mgo.Session) (*mdbstructs.ListShards, error) {
	listShards := mdbstructs.ListShards{}
	err := session.Run(bson.D{{"listShards", "1"}}, &listShards)
	return &listShards, err
}

// Return shards within a sharded cluster using the 'config.shards'
// collection on a config server. This is needed because config servers
// do not have the 'listShards' command. Use .GetListShards() to get
// shards from a mongos instead.
//
// https://docs.mongodb.com/manual/reference/config-database/#config.shards
//
func GetConfigsvrShards(session *mgo.Session) ([]*mdbstructs.Shard, error) {
	shards := []*mdbstructs.Shard{}
	err := session.DB(configDB).C("shards").Find(nil).All(&shards)
	return shards, err
}