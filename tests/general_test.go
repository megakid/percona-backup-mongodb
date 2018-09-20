package test_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/alecthomas/kingpin"
	"github.com/globalsign/mgo"
	"github.com/percona/mongodb-backup/bsonfile"
	"github.com/percona/mongodb-backup/grpc/server"
	"github.com/percona/mongodb-backup/internal/oplog"
	"github.com/percona/mongodb-backup/internal/restore"
	"github.com/percona/mongodb-backup/internal/testutils"
	pb "github.com/percona/mongodb-backup/proto/messages"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/testdata"
	"gopkg.in/mgo.v2/bson"
)

var port, apiPort string
var grpcServerShutdownTimeout = 30

const (
	dbName  = "test"
	colName = "test_col"
)

type cliOptions struct {
	app        *kingpin.Application
	clientID   *string
	tls        *bool
	caFile     *string
	serverAddr *string
}

func TestMain(m *testing.M) {
	log.SetLevel(log.ErrorLevel)
	if os.Getenv("DEBUG") == "1" {
		log.SetLevel(log.DebugLevel)
	}

	if port = os.Getenv("GRPC_PORT"); port == "" {
		port = "10000"
	}
	if apiPort = os.Getenv("GRPC_API_PORT"); apiPort == "" {
		apiPort = "10001"
	}

	os.Exit(m.Run())
}

func TestGlobalWithDaemon(t *testing.T) {
	d, err := testutils.NewGrpcDaemon(context.Background(), t, nil)
	if err != nil {
		t.Fatalf("cannot start a new gRPC daemon/clients group: %s", err)
	}

	tmpDir := path.Join(os.TempDir(), "dump_test")
	os.RemoveAll(tmpDir) // Don't check for errors. The path might not exist
	err = os.MkdirAll(tmpDir, os.ModePerm)
	if err != nil {
		log.Printf("Cannot create temp dir %q, %s", tmpDir, err)
	}

	log.Debug("Getting list of connected clients")
	clientsList := d.MessagesServer.Clients()
	log.Debugf("Clients: %+v\n", clientsList)
	if len(clientsList) != d.ClientsCount() {
		t.Errorf("Want %d connected clients, got %d", d.ClientsCount(), len(clientsList))
	}

	log.Debug("Getting list of clients by replicaset")
	clientsByReplicaset := d.MessagesServer.ClientsByReplicaset()
	log.Debugf("Clients by replicaset: %+v\n", clientsByReplicaset)

	var firstClient *server.Client
	for _, client := range clientsByReplicaset {
		if len(client) == 0 {
			break
		}
		firstClient = client[0]
		break
	}

	log.Debugf("Getting first client status")
	status, err := firstClient.Status()
	log.Debugf("Client status: %+v\n", status)
	if err != nil {
		t.Errorf("Cannot get first client status: %s", err)
	}
	if status.BackupType != pb.BackupType_LOGICAL {
		t.Errorf("The default backup type should be 0 (Logical). Got backup type: %v", status.BackupType)
	}

	log.Debugf("Getting backup source")
	backupSource, err := firstClient.GetBackupSource()
	log.Debugf("Backup source: %+v\n", backupSource)
	if err != nil {
		t.Errorf("Cannot get backup source: %s", err)
	}
	if backupSource == "" {
		t.Error("Received empty backup source")
	}
	log.Printf("Received backup source: %v", backupSource)

	wantbs := map[string]*server.Client{
		"5b9e5545c003eb6bd5e94803": &server.Client{
			ID:             "127.0.0.1:17003",
			NodeType:       pb.NodeType(3),
			NodeName:       "127.0.0.1:17003",
			ClusterID:      "5b9e5548fcaf061d1ed3830d",
			ReplicasetName: "rs1",
			ReplicasetUUID: "5b9e5545c003eb6bd5e94803",
		},
		"5b9e55461f4c8f50f48c6002": &server.Client{
			ID:             "127.0.0.1:17006",
			NodeType:       pb.NodeType(3),
			NodeName:       "127.0.0.1:17006",
			ClusterID:      "5b9e5548fcaf061d1ed3830d",
			ReplicasetName: "rs2",
			ReplicasetUUID: "5b9e55461f4c8f50f48c6002",
		},
		"5b9e5546fcaf061d1ed382ed": &server.Client{
			ID:             "127.0.0.1:17007",
			NodeType:       pb.NodeType(4),
			NodeName:       "127.0.0.1:17007",
			ClusterID:      "",
			ReplicasetName: "csReplSet",
			ReplicasetUUID: "5b9e5546fcaf061d1ed382ed",
		},
	}
	backupSources, err := d.MessagesServer.BackupSourceByReplicaset()
	if err != nil {
		t.Fatalf("Cannot get backup sources by replica set: %s", err)
	}

	okCount := 0
	for _, s := range backupSources {
		for _, bs := range wantbs {
			if s.ID == bs.ID && s.NodeType == bs.NodeType && s.NodeName == bs.NodeName && bs.ReplicasetUUID == s.ReplicasetUUID {
				okCount++
			}
		}
	}

	// Genrate random data so we have something in the oplog
	oplogGeneratorStopChan := make(chan bool)
	session, err := mgo.DialWithInfo(testutils.PrimaryDialInfo(t, testutils.MongoDBShard1ReplsetName))
	if err != nil {
		log.Fatalf("Cannot connect to the DB: %s", err)
	}
	generateDataToBackup(t, session)
	go generateOplogTraffic(t, session, oplogGeneratorStopChan)

	// Start the backup. Backup just the first replicaset (sorted by name) so we can test the restore
	replNames := sortedReplicaNames(backupSources)
	replName := replNames[0]
	client := backupSources[replName]

	fmt.Printf("Replicaset: %s, Client: %s\n", replName, client.NodeName)
	if testing.Verbose() {
		log.Printf("Temp dir for backup: %s", tmpDir)
	}

	err = d.MessagesServer.StartBackup(&pb.StartBackup{
		BackupType:      pb.BackupType_LOGICAL,
		DestinationType: pb.DestinationType_FILE,
		DestinationName: "test",
		DestinationDir:  tmpDir,
		CompressionType: pb.CompressionType_NO_COMPRESSION,
		Cypher:          pb.Cypher_NO_CYPHER,
		OplogStartTime:  time.Now().Unix(),
	})
	if err != nil {
		t.Fatalf("Cannot start backup: %s", err)
	}
	d.MessagesServer.WaitBackupFinish()

	// I want to know how many documents are in the DB. These docs are being generated by the go-routine
	// generateOplogTraffic(). Since we are stopping the log tailer AFTER counting how many docs we have
	// in the DB, the test restore funcion is going to check that after restoring the db and applying the
	// oplog, the max `number` field in the DB sould be higher that maxnumber.Number if the backup
	// is correct

	type maxnumber struct {
		Number int64 `bson:"number"`
	}
	var max maxnumber
	err = session.DB(dbName).C(colName).Find(nil).Sort("-number").Limit(1).One(&max)
	if err != nil {
		log.Fatalf("Cannot get the max 'number' field in %s.%s: %s", dbName, colName, err)
	}

	log.Info("Stopping the oplog tailer")
	err = d.MessagesServer.StopOplogTail()
	if err != nil {
		t.Fatalf("Cannot stop the oplog tailer: %s", err)
	}

	close(oplogGeneratorStopChan)
	d.MessagesServer.WaitOplogBackupFinish()

	t.Log("Skipping restore tests in new replicaset sandbox")

	d.Stop()
}

func TestClientDisconnect(t *testing.T) {
	d, err := testutils.NewGrpcDaemon(context.Background(), t, nil)
	if err != nil {
		t.Fatalf("cannot start a new gRPC daemon/clients group: %s", err)
	}

	clientsCount1 := len(d.MessagesServer.Clients())
	// Disconnect a client to check if the server detects the disconnection immediatelly
	d.Clients()[0].Stop()

	time.Sleep(2 * time.Second)

	clientsCount2 := len(d.MessagesServer.Clients())

	if clientsCount2 >= clientsCount1 {
		t.Errorf("Invalid clients count. Want < %d, got %d", clientsCount1, clientsCount2)
	}
	d.Stop()
}

func testRestore(t *testing.T, session *mgo.Session, dir string) {
	log.Println("Starting mongo restore")
	if err := session.DB(dbName).C(colName).DropCollection(); err != nil {
		t.Fatalf("Cannot clean up %s.%s collection: %s", dbName, colName, err)
	}
	session.Refresh()

	count, err := session.DB(dbName).C(colName).Find(nil).Count()
	if err != nil {
		t.Fatalf("Cannot count number of rows in the test collection %s: %s", colName, err)
	}

	if count > 0 {
		t.Fatalf("Invalid rows count in the test collection %s. Got %d, want 0", colName, count)
	}

	input := &restore.MongoRestoreInput{
		// this file was generated with the dump pkg
		Archive:  path.Join(dir, "test.dump"),
		DryRun:   false,
		Host:     testutils.MongoDBHost,
		Port:     testutils.MongoDBShard1PrimaryPort,
		Username: testutils.MongoDBUser,
		Password: testutils.MongoDBPassword,
		Gzip:     false,
		Oplog:    false,
		Threads:  1,
		Reader:   nil,
		// A real restore would be applied to a just created and empty instance and it should be
		// configured to run without user authentication.
		// Since we are running a sandbox and we already have user and roles and authentication
		// is enableb, we need to skip restoring users and roles, otherwise the test will fail
		// since there are already users/roles in the admin db. Also we cannot delete admin db
		// because we are using it.
		SkipUsersAndRoles: true,
	}

	r, err := restore.NewMongoRestore(input)
	if err != nil {
		t.Errorf("Cannot instantiate mongo restore instance: %s", err)
		t.FailNow()
	}

	if err := r.Start(); err != nil {
		t.Errorf("Cannot start restore: %s", err)
	}

	if err := r.Wait(); err != nil {
		t.Errorf("Error while trying to restore: %s", err)
	}

	count, err = session.DB(dbName).C(colName).Find(nil).Count()
	if err != nil {
		t.Fatalf("Cannot count number of rows in the test collection %q after restore: %s", colName, err)
	}

	if count < 100 {
		t.Fatalf("Invalid rows count in the test collection %s. Got %d, want > 100", colName, count)
	}

}

func testOplogApply(t *testing.T, session *mgo.Session, dir string, minOplogDocs int64) {
	log.Print("Starting oplog apply test")
	oplogFile := path.Join(dir, "test.oplog")
	reader, err := bsonfile.OpenFile(oplogFile)
	if err != nil {
		t.Errorf("Cannot open oplog dump file %q: %s", oplogFile, err)
		return
	}

	// Replay the oplog
	oa, err := oplog.NewOplogApply(session, reader)
	if err != nil {
		t.Errorf("Cannot instantiate the oplog applier: %s", err)
	}

	if err := oa.Run(); err != nil {
		t.Errorf("Error while running the oplog applier: %s", err)
	}
	type maxnumber struct {
		Number int64 `bson:"number"`
	}
	var max maxnumber
	session.DB(dbName).C(colName).Find(nil).Sort("-number").Limit(1).One(&max)
	if max.Number < minOplogDocs {
		t.Errorf("Invalid number of documents in the oplog. Got %d, want > %d", max.Number, minOplogDocs)
	}
}

func sortedReplicaNames(replicas map[string]*server.Client) []string {
	a := []string{}
	for key, _ := range replicas {
		a = append(a, key)
	}
	sort.Strings(a)
	return a
}

func generateDataToBackup(t *testing.T, session *mgo.Session) {
	// Don't check for error because the collection might not exist.
	session.DB(dbName).C(colName).DropCollection()
	session.DB(dbName).C(colName).EnsureIndexKey("number")
	session.Refresh()

	for i := 0; i < 100; i++ {
		err := session.DB(dbName).C(colName).Insert(bson.M{"number": i})
		if err != nil {
			t.Fatalf("Cannot insert data to back up: %s", err)
		}
	}
}

func generateOplogTraffic(t *testing.T, session *mgo.Session, stop chan bool) {
	ticker := time.NewTicker(10 * time.Millisecond)
	i := int64(1000)
	for {
		select {
		case <-stop:
			ticker.Stop()
			return
		case <-ticker.C:
			err := session.DB(dbName).C(colName).Insert(bson.M{"number": i})
			if err != nil {
				t.Logf("insert to test.test failed: %v", err.Error())
			}
			i++
		}
	}
}
func runAgentsGRPCServer(grpcServer *grpc.Server, lis net.Listener, shutdownTimeout int, stopChan chan interface{}, wg *sync.WaitGroup) {
	go func() {
		err := grpcServer.Serve(lis)
		if err != nil {
			log.Printf("Cannot start agents gRPC server: %s", err)
		}
		log.Println("Stopping server " + lis.Addr().String())
		wg.Done()
	}()

	go func() {
		<-stopChan
		log.Printf("Gracefuly stopping server at %s", lis.Addr().String())
		// Try to Gracefuly stop the gRPC server.
		c := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			c <- struct{}{}
		}()

		// If after shutdownTimeout the server hasn't stop, just kill it.
		select {
		case <-c:
			return
		case <-time.After(time.Duration(shutdownTimeout) * time.Second):
			grpcServer.Stop()
		}
	}()
}

func getAPIConn(opts *cliOptions) (*grpc.ClientConn, error) {
	var grpcOpts []grpc.DialOption

	if opts.serverAddr == nil || *opts.serverAddr == "" {
		return nil, fmt.Errorf("Invalid server address (nil or empty)")
	}

	if opts.tls != nil && *opts.tls {
		if opts.caFile != nil && *opts.caFile == "" {
			*opts.caFile = testdata.Path("ca.pem")
		}
		creds, err := credentials.NewClientTLSFromFile(*opts.caFile, "")
		if err != nil {
			log.Fatalf("Failed to create TLS credentials %v", err)
		}
		grpcOpts = append(grpcOpts, grpc.WithTransportCredentials(creds))
	} else {
		grpcOpts = append(grpcOpts, grpc.WithInsecure())
	}

	conn, err := grpc.Dial(*opts.serverAddr, grpcOpts...)
	if err != nil {
		log.Fatalf("fail to dial: %v", err)
	}
	return conn, err
}