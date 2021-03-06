// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//  http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package manager

import (
	"fmt"
	"github.com/couchbase/indexing/secondary/logging"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/dataport"
	data "github.com/couchbase/indexing/secondary/protobuf/data"
	protobuf "github.com/couchbase/indexing/secondary/protobuf/projector"
	"net"
	"sync"
)

///////////////////////////////////////////////////////
// Type Definition
///////////////////////////////////////////////////////

//
// Callback function for handling mutation commands from the mutation source (projector).
//	Upsert         		- data command
//	Deletion            - data command
//	UpsertDeletion      - data command
//	Sync                - control command
//	DropData            - control command
//	StreamBegin         - control command
//	StreamEnd           - control command
//	Snapshot            - control command
//
type MutationHandler interface {
	HandleUpsert(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleDeletion(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleUpsertDeletion(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleSync(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleDropData(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleStreamBegin(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleStreamEnd(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleSnapshot(streamId common.StreamId, bucket string, vbucket uint32, vbuuid uint64, kv *data.KeyVersions, offset int)
	HandleConnectionError(streamId common.StreamId, err dataport.ConnectionError)
}

//
// Callback for handling stream administration for the remote mutation source (projector).   There are mutliple
// mutation sources per stream.   The StreamAdmin needs to encapsulate topology of the mutation sources.
//
type StreamAdmin interface {
	AddIndexToStream(streamId common.StreamId, bucket []string, instances []*protobuf.Instance, requestTs []*common.TsVbuuid) error
	DeleteIndexFromStream(streamId common.StreamId, bucket []string, instances []uint64) error
	RepairEndpointForStream(streamId common.StreamId, bucketVbnosMap map[string][]uint16, endpoint string) error
	RestartStreamIfNecessary(streamId common.StreamId, timestamps []*common.TsVbuuid) error
	Initialize(monitor *StreamMonitor)
}

//
// StreamManager for managing stream for mutation consumer.
//
type StreamManager struct {
	streams    map[common.StreamId]*Stream
	handler    MutationHandler
	admin      StreamAdmin
	indexMgr   *IndexManager
	topologies map[string]*IndexTopology
	monitor    *StreamMonitor

	mutex    sync.Mutex
	isClosed bool
	stopch   chan bool
}

///////////////////////////////////////////////////////
// public function - Stream Manager
///////////////////////////////////////////////////////

//
// Create new stream managaer
//
func NewStreamManager(indexMgr *IndexManager, handler MutationHandler, admin StreamAdmin, monitor *StreamMonitor) (*StreamManager, error) {

	mgr := &StreamManager{streams: make(map[common.StreamId]*Stream),
		handler:    handler,
		indexMgr:   indexMgr,
		admin:      admin,
		stopch:     make(chan bool),
		topologies: make(map[string]*IndexTopology),
		isClosed:   false,
		monitor:    monitor}

	if mgr.monitor != nil {
		mgr.monitor.Start()
	}

	return mgr, nil
}

//
// Close all the streams.  This will close the connection to the mutation source and subsequently,
// each mutation source will clean up on their side.
//
func (s *StreamManager) Close() {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return
	}

	for _, stream := range s.streams {
		stream.Close()
		s.closeStreamNoLock(stream.id)
	}

	if s.monitor != nil {
		s.monitor.Close()
	}

	close(s.stopch)
	s.isClosed = true
}

//
// Is all the stream closed?
//
func (s *StreamManager) IsClosed() bool {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.isClosed
}

//
// Start stream manager for processing topology changes
//
func (s *StreamManager) StartHandlingTopologyChange() {

	if !s.IsClosed() {
		logging.Debugf("StreamManager.StartHandlingTopologyChange(): start")
		go s.run()
	}
}

//
// Start a stream for listening only.  This will not trigger the mutation source to start
// streaming mutations.   Need to call AddIndexForBucket() or AddIndexForAllBuckets()
// to kick off the mutation source to start streaming the mutations for indexes in bucket(s).
//
func (s *StreamManager) StartStream(streamId common.StreamId) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	logging.Debugf("StreamManager.StartStream(): start")

	if s.isClosed {
		return nil
	}

	// Verify if the stream is already open.  Just an no-op.
	if stream, ok := s.streams[streamId]; ok && stream.status {
		logging.Debugf("StreamManager.StartStream(): stream %v already started", streamId)
		return nil
	}

	// Create a new stream.  This will prepare the reciever to be ready for receving mutation.
	port := getPortForStreamId(streamId)
	stream, err := newStream(streamId, port, s.handler)
	if err != nil {
		return err
	}

	err = stream.start()
	if err != nil {
		return err
	}
	logging.Debugf("StreamManager.StartStream(): stream %v started successfully on port %v", streamId, port)

	s.streams[streamId] = stream
	stream.status = true
	return nil
}

//
// Kick off the mutation source to start streaming mutation for all buckets.
// If stream has already open for a specific bucket, then this function will
// ignore this error.
//
func (s *StreamManager) AddIndexForAllBuckets(streamId common.StreamId) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return nil
	}

	buckets, err := s.getBucketWithIndexes()
	if err != nil {
		return err
	}

	return s.AddIndexForBuckets(streamId, buckets)
}

//
// Kick off the mutation source to start streaming mutation for given buckets.
// If stream has already open for a specific bucket, then this function will
// ignore this error.
//
func (s *StreamManager) AddIndexForBuckets(streamId common.StreamId, buckets []string) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return nil
	}

	// Verify if the stream is already open
	stream, ok := s.streams[streamId]
	if !ok || !stream.status {
		return NewError2(ERROR_STREAM_NOT_OPEN, STREAM)
	}

	var allInstances []*protobuf.Instance = nil
	allTopologies := make(map[string]*IndexTopology)

	for _, bucket := range buckets {

		// Start the timer before start the stream.  Once the stream comes, the timer needs to be ready.
		s.indexMgr.getTimer().start(streamId, bucket)

		// Genereate the index instance protobuf messages based on distribution topology
		port := getPortForStreamId(streamId)
		addr, err := s.getAddrForPort(port)
		if err != nil {
			return err
		}

		// Get the index topology for this index
		instances, topology, err := GetTopologyAsInstanceProtoMsg(s.indexMgr, bucket, addr)
		if err != nil {
			return err
		}

		if len(instances) != 0 {
			allInstances = append(allInstances, instances...)
			allTopologies[bucket] = topology
		}
	}

	if err := s.admin.AddIndexToStream(streamId, buckets, allInstances, nil); err != nil {
		return err
	}

	return nil
}

//
// Delete Index For Buckets
//
func (s *StreamManager) DeleteIndexForBuckets(streamId common.StreamId, buckets []string) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return nil
	}

	// Verify if the stream is already open
	stream, ok := s.streams[streamId]
	if !ok || !stream.status {
		return NewError2(ERROR_STREAM_NOT_OPEN, STREAM)
	}

	// Genereate the index instance protobuf messages based on distribution topology
	instances, err := GetAllDeletedIndexInstancesId(s.indexMgr, buckets)
	if err != nil {
		return err
	}

	if err := s.admin.DeleteIndexFromStream(streamId, buckets, instances); err != nil {
		return err
	}

	return nil
}

//
// Restart specific vbucket in the stream based on the given timestamp.
//
func (s *StreamManager) RestartStreamIfNecessary(streamId common.StreamId, timestamps []*common.TsVbuuid) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return nil
	}

	return s.admin.RestartStreamIfNecessary(streamId, timestamps)
}

//
// Close a particular stream. - todo
//
func (s *StreamManager) CloseStream(streamId common.StreamId) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return nil
	}

	return s.closeStreamNoLock(streamId)
}

//
// Add index instances to a stream.  The list of instances can be coming from different index definitions, but the
// index definitions must be coming from the same bucket.
//
func (s *StreamManager) addIndexInstances(streamId common.StreamId, bucket string, instances []*protobuf.Instance) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	logging.Debugf("StreamManager.addIndexInstances() bucket %v", bucket)

	if s.isClosed {
		return nil
	}

	//if stream not open, return error
	stream, ok := s.streams[streamId]
	if !ok || !stream.status {
		return NewError2(ERROR_STREAM_NOT_OPEN, STREAM)
	}

	// Start the timer before start the stream.  Once the stream comes, the timer needs to be ready.
	s.indexMgr.getTimer().start(streamId, bucket)

	// Pass the new topology to the data source
	if err := s.admin.AddIndexToStream(streamId, []string{bucket}, instances, nil); err != nil {
		return err
	}

	return nil
}

//
// Remove index instances from stream
//
func (s *StreamManager) removeIndexInstances(streamId common.StreamId, bucket string, instances []uint64) error {

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if s.isClosed {
		return nil
	}

	//if stream not open, return error
	stream, ok := s.streams[streamId]
	if !ok || !stream.status {
		return NewError2(ERROR_STREAM_NOT_OPEN, STREAM)
	}

	// Remove those index instances from the stream
	if err := s.admin.DeleteIndexFromStream(streamId, []string{bucket}, instances); err != nil {
		return err
	}

	return nil
}

///////////////////////////////////////////////////////
// package-local function - Stream Manager
///////////////////////////////////////////////////////

//
// Get the stream
//
func (s *StreamManager) getStream(streamId common.StreamId) *Stream {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	return s.streams[streamId]
}

func (s *StreamManager) getAddrForPort(port string) (string, error) {
	addrProvider := s.indexMgr.getServiceAddrProvider()

	host, err := addrProvider.GetLocalServiceHost("indexAdmin")
	if err != nil {
		return "", err
	}

	return net.JoinHostPort(host, port), nil
}

///////////////////////////////////////////////////////
// private function - StreamManager
///////////////////////////////////////////////////////

func (s *StreamManager) closeStreamNoLock(streamId common.StreamId) error {

	//if stream not open, no-op
	stream, ok := s.streams[streamId]
	if !ok || !stream.status {
		return nil
	}

	// Stop the timer for all the bucket for this stream
	s.indexMgr.getTimer().stopForStream(streamId)

	/*
	   if err := CloseStreamForBucket(streamId); err != nil {
	       return err
	   }
	*/

	// book keeping
	delete(s.streams, streamId)

	stream.status = false
	return nil
}

//////////////////////////////////////////////////////////
// private function - bootstrap + topology change listener
//////////////////////////////////////////////////////////

//
// Go-routine to bootstrap projectors for shared stream, as well as continous
// maintanence of the shared stream.  It listens to any new topology update and
// update the projector in response to topology update.
//
func (s *StreamManager) run() {

	// register to index manager for receiving topology change
	changeCh, err := s.indexMgr.StartListenTopologyUpdate("Stream Manager")
	if err != nil {
		panic(fmt.Sprintf("StreamManager.run(): Fail to listen to topology changes from repository.  Error = %v", err))
	}

	// load topology
	if err := s.loadTopology(); err != nil {
		panic(fmt.Sprintf("StreamManager.run(): Fail to load topology from repository.  Error = %v", err))
	}

	// initialize stream
	if err := s.initializeMaintenanceStream(); err != nil {
		panic(fmt.Sprintf("StreamManager.run(): Fail to initialize maintenance stream.  Error = %v", err))
	}

	for {
		select {
		case data, ok := <-changeCh:
			if !ok {
				logging.Debugf("StreamManager.run(): topology change channel is closed.  Terminates.")
				return
			}

			func() {
				defer func() {
					if r := recover(); r != nil {
						logging.Warnf("panic in StreamManager.run() : %s.  Ignored.", r)
					}
				}()

				topology, err := unmarshallIndexTopology(data.([]byte))
				if err != nil {
					logging.Errorf("StreamManager.run(): unable to unmarshall topology.  Topology change is ignored by stream manager.")
				} else {
					err := s.handleTopologyChange(topology)
					if err != nil {
						logging.Errorf("StreamManager.run(): receive error from handleTopologyChange.  Error = %v.  Ignore", err)
					}
				}
			}()

		case <-s.stopch:
			return
		}
	}
}

//////////////////////////////////////////////////////////
// private function - bootstrap
//////////////////////////////////////////////////////////

//
// Initialize the maintenance stream from the current topology.
//
func (s *StreamManager) initializeMaintenanceStream() error {

	logging.Debugf("StreamManager.initializeMaintenanceStream():Start()")

	// Notify the projector to start the incremental stream
	if err := s.StartStream(common.MAINT_STREAM); err != nil {
		return err
	}

	// Get the list of buckets
	buckets, err := s.getBucketWithIndexes()
	if err != nil {
		return err
	}

	if buckets != nil {
		if err := s.AddIndexForBuckets(common.MAINT_STREAM, buckets); err != nil {
			return err
		}

		if err := s.DeleteIndexForBuckets(common.MAINT_STREAM, buckets); err != nil {
			return err
		}
	}

	return nil
}

//
// Get the list of buckets that have indexes.
//
func (s *StreamManager) getBucketWithIndexes() ([]string, error) {

	// Get Global Topology
	globalTop, err := s.indexMgr.GetGlobalTopology()
	if err != nil {
		// TODO: Differentiate the case where global topology does not exist
		return nil, nil
		//return nil, err
	}

	// Create a topology map
	var buckets []string = nil

	for _, key := range globalTop.TopologyKeys {
		bucket := getBucketFromTopologyKey(key)
		buckets = append(buckets, bucket)
	}

	return buckets, nil
}

//
// Load topology from repository
//
func (s *StreamManager) loadTopology() error {

	// Get Global Topology
	globalTop, err := s.indexMgr.GetGlobalTopology()
	if err != nil {
		// TODO: Differentiate the case where global topology does not exist
		return nil
		//return err
	}

	for _, key := range globalTop.TopologyKeys {
		bucket := getBucketFromTopologyKey(key)
		topology, err := s.indexMgr.GetTopologyByBucket(bucket)
		if err != nil {
			return err
		}
		s.topologies[bucket] = topology
	}

	return nil
}

//////////////////////////////////////////////////////////
// private function - handle topology change
//////////////////////////////////////////////////////////

//
// Handle Topology changes for both maintenance and init streams
//
func (s *StreamManager) handleTopologyChange(newTopology *IndexTopology) error {

	logging.Debugf("StreamManager.handleTopologyChange()")

	if err := s.handleTopologyChangeForMaintStream(newTopology); err != nil {
		return err
	}

	if err := s.handleTopologyChangeForInitStream(newTopology); err != nil {
		return err
	}

	// Update the topology
	s.topologies[newTopology.Bucket] = newTopology

	return nil
}

//
// This function responds to topology change for maintenance stream.
//
func (s *StreamManager) handleTopologyChangeForMaintStream(newTopology *IndexTopology) error {

	stream, ok := s.streams[common.MAINT_STREAM]
	if !ok || !stream.status {
		return nil
	}
	logging.Debugf("StreamManager.handleTopologyChangeForMaintStream(): new topology for bucket %v version %v ",
		newTopology.Bucket, newTopology.Version)

	oldTopology, ok := s.topologies[newTopology.Bucket]
	if !ok {
		logging.Debugf("StreamManager.handleTopologyChangeForMaintStream(): old topology for bucket %v", newTopology.Bucket)
		oldTopology = nil
	} else {
		logging.Debugf("StreamManager.handleTopologyChangeForMaintStream(): old topology exist for bucket %. Version %v ",
			oldTopology.Bucket, oldTopology.Version)
	}

	// Add index instances
	// 1) index instance moved from CREATED state to READY state.
	if err := s.handleAddInstances(common.MAINT_STREAM, newTopology.Bucket, oldTopology, newTopology,
		[]common.IndexState{common.INDEX_STATE_CREATED},
		[]common.IndexState{common.INDEX_STATE_READY}); err != nil {
		return err
	}

	// Delete index instances
	// 1) index instance moved from any state to DELETED state
	if err := s.handleDeleteInstances(common.MAINT_STREAM, newTopology.Bucket, oldTopology, newTopology,
		[]common.IndexState{common.INDEX_STATE_ACTIVE, common.INDEX_STATE_READY},
		[]common.IndexState{common.INDEX_STATE_DELETED}); err != nil {
		return err
	}

	return nil
}

//
// This function responds to topology change for init stream.
//
func (s *StreamManager) handleTopologyChangeForInitStream(newTopology *IndexTopology) error {

	stream, ok := s.streams[common.INIT_STREAM]
	if !ok || !stream.status {
		return nil
	}

	oldTopology, ok := s.topologies[newTopology.Bucket]
	if !ok {
		oldTopology = nil
	}

	// Add index instances
	// 1) index instance moved from CREATED state to READY state.
	if err := s.handleAddInstances(common.INIT_STREAM, newTopology.Bucket, oldTopology, newTopology,
		[]common.IndexState{common.INDEX_STATE_CREATED},
		[]common.IndexState{common.INDEX_STATE_READY}); err != nil {
		return err
	}

	// Delete index instances
	// 1) index instance moved from any state to DELETED state
	// 2) index insntace moved from any state to ACTIVE state
	if err := s.handleDeleteInstances(common.INIT_STREAM, newTopology.Bucket, oldTopology, newTopology, nil,
		[]common.IndexState{common.INDEX_STATE_DELETED, common.INDEX_STATE_ACTIVE}); err != nil {
		return err
	}

	return nil
}

//
// This function computes topology changes.
//
func (s *StreamManager) handleAddInstances(
	streamId common.StreamId,
	bucket string,
	oldTopology *IndexTopology,
	newTopology *IndexTopology,
	fromState []common.IndexState,
	toState []common.IndexState) error {

	logging.Debugf("StreamManager.handleAddInstances()")

	if oldTopology != nil && oldTopology.Version == newTopology.Version {
		logging.Debugf("StreamManager.handleAddInstances(): new topology version = %v, old topology version = %v.",
			newTopology.Version, oldTopology.Version)
		return nil
	}

	var changes []*changeRecord = nil

	for _, newDefn := range newTopology.Definitions {
		if oldTopology != nil {
			oldDefn := oldTopology.FindIndexDefinition(bucket, newDefn.Name)
			changes = append(changes, s.addInstancesToChangeList(oldDefn, &newDefn, fromState, toState)...)
		} else {
			changes = append(changes, s.addInstancesToChangeList(nil, &newDefn, fromState, toState)...)
		}
	}

	if len(changes) > 0 {
		port := getPortForStreamId(streamId)
		addr, err := s.getAddrForPort(port)
		if err != nil {
			return err
		}

		instances, err := GetChangeRecordAsProtoMsg(s.indexMgr, changes, addr)
		if err != nil {
			return err
		}
		return s.addIndexInstances(streamId, bucket, instances)
	} else {
		logging.Debugf("StreamManager.handleAddInstances(): no new changes")
	}

	return nil
}

//
// This function finds the difference between two index definitons (in topology).
// This takes into account the state transition of the index instances within
// the index definitions (e.g. from CREATED to INITIAL).  If there is no matching
// index instance in the old index definition,  then fromState is ignored, and
// index instances (from new index definition) will be added as long as it is in toState.
//
func (s *StreamManager) addInstancesToChangeList(
	oldDefn *IndexDefnDistribution,
	newDefn *IndexDefnDistribution,
	fromStates []common.IndexState,
	toStates []common.IndexState) []*changeRecord {

	var changes []*changeRecord = nil

	logging.Debugf("StreamManager.addInstancesToChangeList(): defn '%v'", newDefn.Name)

	for _, newInst := range newDefn.Instances {
		add := s.inState(common.IndexState(newInst.State), toStates)
		logging.Debugf("StreamManager.addInstancesToChangeList(): found new instance '%v' in state %v",
			newInst.InstId, newInst.State)

		if oldDefn != nil {
			for _, oldInst := range oldDefn.Instances {
				if newInst.InstId == oldInst.InstId {
					if s.inState(common.IndexState(oldInst.State), fromStates) {
						logging.Debugf("StreamManager.addInstancesToChangeList(): found old instance '%v' in state %v",
							oldInst.InstId, oldInst.State)
					}
					add = add && s.inState(common.IndexState(oldInst.State), fromStates) &&
						oldInst.State != newInst.State
				}
			}
		}

		if add {
			logging.Debugf("StreamManager.addInstancesToChangeList(): adding inst '%v' to change list.", newInst.InstId)
			change := &changeRecord{definition: newDefn, instance: &newInst}
			changes = append(changes, change)
		}
	}

	return changes
}

//
// Return true if 'state' is in one of the possible states.  If the possible states are not given (nil),
// then return true.
//
func (s *StreamManager) inState(state common.IndexState, possibleStates []common.IndexState) bool {

	if possibleStates == nil {
		return true
	}

	for _, possible := range possibleStates {
		if state == possible {
			return true
		}
	}

	return false
}

//
// This function computes topology changes.
//
func (s *StreamManager) handleDeleteInstances(
	streamId common.StreamId,
	bucket string,
	oldTopology *IndexTopology,
	newTopology *IndexTopology,
	fromState []common.IndexState,
	toState []common.IndexState) error {

	if oldTopology == nil || oldTopology.Version == newTopology.Version {
		return nil
	}

	var changes []*changeRecord = nil

	for _, newDefn := range newTopology.Definitions {
		if oldTopology != nil {
			oldDefn := oldTopology.FindIndexDefinition(newDefn.Bucket, newDefn.Name)
			changes = append(changes, s.addInstancesToChangeList(oldDefn, &newDefn, fromState, toState)...)
		} else {
			changes = append(changes, s.addInstancesToChangeList(nil, &newDefn, fromState, toState)...)
		}
	}

	var toBeDeleted []uint64 = nil
	for _, change := range changes {
		logging.Debugf("StreamManager.handleDeleteInstances(): adding inst '%v' to change list.", change.instance.InstId)
		toBeDeleted = append(toBeDeleted, change.instance.InstId)
	}

	logging.Debugf("StreamManager.handleDeleteInstances(): len(toBeDeleted) '%v'", len(toBeDeleted))
	return s.removeIndexInstances(streamId, bucket, toBeDeleted)
}
