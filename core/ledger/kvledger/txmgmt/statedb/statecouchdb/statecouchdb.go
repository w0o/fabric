/*
Copyright IBM Corp. 2016, 2017 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package statecouchdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statedb"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/version"
	"github.com/hyperledger/fabric/core/ledger/ledgerconfig"
	"github.com/hyperledger/fabric/core/ledger/util/couchdb"
)

var logger = flogging.MustGetLogger("statecouchdb")

var compositeKeySep = []byte{0x00}
var lastKeyIndicator = byte(0x01)

var binaryWrapper = "valueBytes"

//querySkip is implemented for future use by query paging
//currently defaulted to 0 and is not used
var querySkip = 0

// VersionedDBProvider implements interface VersionedDBProvider
type VersionedDBProvider struct {
	couchInstance *couchdb.CouchInstance
	databases     map[string]*VersionedDB
	mux           sync.Mutex
	openCounts    uint64
}

//CommittedVersions contains maps of committedVersions and revisionNumbers
type CommittedVersions struct {
	committedVersions map[statedb.CompositeKey]*version.Height
	revisionNumbers   map[statedb.CompositeKey]string
	committedValues   map[statedb.CompositeKey][]byte
}

// NewVersionedDBProvider instantiates VersionedDBProvider
func NewVersionedDBProvider() (*VersionedDBProvider, error) {
	logger.Debugf("constructing CouchDB VersionedDBProvider")
	couchDBDef := couchdb.GetCouchDBDefinition()
	couchInstance, err := couchdb.CreateCouchInstance(couchDBDef.URL, couchDBDef.Username, couchDBDef.Password,
		couchDBDef.MaxRetries, couchDBDef.MaxRetriesOnStartup, couchDBDef.RequestTimeout)
	if err != nil {
		return nil, err
	}

	return &VersionedDBProvider{couchInstance, make(map[string]*VersionedDB), sync.Mutex{}, 0}, nil
}

// GetDBHandle gets the handle to a named database
func (provider *VersionedDBProvider) GetDBHandle(dbName string) (statedb.VersionedDB, error) {
	provider.mux.Lock()
	defer provider.mux.Unlock()

	vdb := provider.databases[dbName]
	if vdb == nil {
		var err error
		vdb, err = newVersionedDB(provider.couchInstance, dbName)
		if err != nil {
			return nil, err
		}
		provider.databases[dbName] = vdb
	}
	return vdb, nil
}

// Close closes the underlying db instance
func (provider *VersionedDBProvider) Close() {
	// No close needed on Couch
}

// VersionedDB implements VersionedDB interface
type VersionedDB struct {
	db            *couchdb.CouchDatabase
	dbName        string
	committedData *CommittedVersions
}

// newVersionedDB constructs an instance of VersionedDB
func newVersionedDB(couchInstance *couchdb.CouchInstance, dbName string) (*VersionedDB, error) {
	// CreateCouchDatabase creates a CouchDB database object, as well as the underlying database if it does not exist
	db, err := couchdb.CreateCouchDatabase(*couchInstance, dbName)
	if err != nil {
		return nil, err
	}
	versionMap := make(map[statedb.CompositeKey]*version.Height)
	revMap := make(map[statedb.CompositeKey]string)
	valMap := make(map[statedb.CompositeKey][]byte)

	committedData := &CommittedVersions{committedVersions: versionMap, revisionNumbers: revMap, committedValues: valMap}

	return &VersionedDB{db, dbName, committedData}, nil
}

// Open implements method in VersionedDB interface
func (vdb *VersionedDB) Open() error {
	// no need to open db since a shared couch instance is used
	return nil
}

// Close implements method in VersionedDB interface
func (vdb *VersionedDB) Close() {
	// no need to close db since a shared couch instance is used
}

// ValidateKey implements method in VersionedDB interface
func (vdb *VersionedDB) ValidateKey(key string) error {
	if !utf8.ValidString(key) {
		return fmt.Errorf("Key should be a valid utf8 string: [%x]", key)
	}
	return nil
}

// GetState implements method in VersionedDB interface
func (vdb *VersionedDB) GetState(namespace string, key string) (*statedb.VersionedValue, error) {
	logger.Debugf("GetState(). ns=%s, key=%s", namespace, key)

	compositeKeyStruct := statedb.CompositeKey{Namespace: namespace, Key: key}
	returnValue, keyFound := vdb.committedData.committedValues[compositeKeyStruct]

	if keyFound {
		returnVersion, ok := vdb.committedData.committedVersions[compositeKeyStruct]
		if ok {
			return &statedb.VersionedValue{Value: returnValue, Version: returnVersion}, nil
		}
		// what? Shouldn't happen. But fetch the data without complaining anyway.
	}

	compositeKey := constructCompositeKey(namespace, key)

	couchDoc, _, err := vdb.db.ReadDoc(string(compositeKey))
	if err != nil {
		return nil, err
	}
	if couchDoc == nil {
		return nil, nil
	}

	//remove the data wrapper and return the value and version
	returnValue, returnVersion := removeDataWrapper(couchDoc.JSONValue, couchDoc.Attachments)

	return &statedb.VersionedValue{Value: returnValue, Version: &returnVersion}, nil
}

// GetVersion implements method in VersionedDB interface
func (vdb *VersionedDB) GetVersion(namespace string, key string) (*version.Height, error) {

	compositeKey := statedb.CompositeKey{Namespace: namespace, Key: key}

	returnVersion, keyFound := vdb.committedData.committedVersions[compositeKey]

	if !keyFound {

		couchDBCompositeKey := constructCompositeKey(namespace, key)
		couchDoc, _, err := vdb.db.ReadDoc(string(couchDBCompositeKey))
		if err == nil {
			return nil, nil
		}

		versionData := &couchdb.Doc{}
		err = json.Unmarshal(couchDoc.JSONValue, &versionData)
		if err != nil {
			logger.Errorf("Failed to unmarshal version data %s\n", err.Error())
			return nil, err
		}

		if versionData.Version == "" {
			return nil, nil
		}

		//create an array containing the blockNum and txNum
		versionArray := strings.Split(versionData.Version, ":")

		//convert the blockNum from String to unsigned int
		blockNum, _ := strconv.ParseUint(versionArray[0], 10, 64)

		//convert the txNum from String to unsigned int
		txNum, _ := strconv.ParseUint(versionArray[1], 10, 64)

		//create the version based on the blockNum and txNum
		returnVersion = version.NewHeight(blockNum, txNum)

	}

	return returnVersion, nil
}

func removeDataWrapper(wrappedValue []byte, attachments []*couchdb.Attachment) ([]byte, version.Height) {

	//initialize the return value
	returnValue := []byte{}

	//initialize a default return version
	returnVersion := version.NewHeight(0, 0)

	//create a generic map for the json
	jsonResult := make(map[string]interface{})

	//unmarshal the selected json into the generic map
	decoder := json.NewDecoder(bytes.NewBuffer(wrappedValue))
	decoder.UseNumber()
	_ = decoder.Decode(&jsonResult)

	// handle binary or json data
	if jsonResult[dataWrapper] == nil && attachments != nil { // binary attachment
		// get binary data from attachment
		for _, attachment := range attachments {
			if attachment.Name == binaryWrapper {
				returnValue = attachment.AttachmentBytes
			}
		}
	} else {
		//place the result json in the data key
		returnMap := jsonResult[dataWrapper]

		//marshal the mapped data.   this wrappers the result in a key named "data"
		returnValue, _ = json.Marshal(returnMap)

	}

	//create an array containing the blockNum and txNum
	versionArray := strings.Split(fmt.Sprintf("%s", jsonResult["version"]), ":")

	//convert the blockNum from String to unsigned int
	blockNum, _ := strconv.ParseUint(versionArray[0], 10, 64)

	//convert the txNum from String to unsigned int
	txNum, _ := strconv.ParseUint(versionArray[1], 10, 64)

	//create the version based on the blockNum and txNum
	returnVersion = version.NewHeight(blockNum, txNum)

	return returnValue, *returnVersion

}

// GetStateMultipleKeys implements method in VersionedDB interface
func (vdb *VersionedDB) GetStateMultipleKeys(namespace string, keys []string) ([]*statedb.VersionedValue, error) {

	var compositeKeys []*statedb.CompositeKey
	for _, key := range keys {
		compositeKeys = append(compositeKeys, &statedb.CompositeKey{Namespace: namespace, Key: key})
	}
	vdb.LoadCommittedValues(compositeKeys)
	defer vdb.ClearCachedVersions()

	vals := make([]*statedb.VersionedValue, len(keys))
	for i, key := range keys {
		val, err := vdb.GetState(namespace, key)
		if err != nil {
			return nil, err
		}
		vals[i] = val
	}
	return vals, nil

}

// GetStateRangeScanIterator implements method in VersionedDB interface
// startKey is inclusive
// endKey is exclusive
func (vdb *VersionedDB) GetStateRangeScanIterator(namespace string, startKey string, endKey string) (statedb.ResultsIterator, error) {

	//Get the querylimit from core.yaml
	queryLimit := ledgerconfig.GetQueryLimit()

	compositeStartKey := constructCompositeKey(namespace, startKey)
	compositeEndKey := constructCompositeKey(namespace, endKey)
	if endKey == "" {
		compositeEndKey[len(compositeEndKey)-1] = lastKeyIndicator
	}
	queryResult, err := vdb.db.ReadDocRange(string(compositeStartKey), string(compositeEndKey), queryLimit, querySkip)
	if err != nil {
		logger.Debugf("Error calling ReadDocRange(): %s\n", err.Error())
		return nil, err
	}
	logger.Debugf("Exiting GetStateRangeScanIterator")
	return newKVScanner(namespace, *queryResult), nil

}

// ExecuteQuery implements method in VersionedDB interface
func (vdb *VersionedDB) ExecuteQuery(namespace, query string) (statedb.ResultsIterator, error) {

	//Get the querylimit from core.yaml
	queryLimit := ledgerconfig.GetQueryLimit()

	queryString, err := ApplyQueryWrapper(namespace, query, queryLimit, 0)
	if err != nil {
		logger.Debugf("Error calling ApplyQueryWrapper(): %s\n", err.Error())
		return nil, err
	}

	queryResult, err := vdb.db.QueryDocuments(queryString)
	if err != nil {
		logger.Debugf("Error calling QueryDocuments(): %s\n", err.Error())
		return nil, err
	}
	logger.Debugf("Exiting ExecuteQuery")
	return newQueryScanner(*queryResult), nil
}

// ApplyUpdates implements method in VersionedDB interface
func (vdb *VersionedDB) ApplyUpdates(batch *statedb.UpdateBatch, height *version.Height) error {

	//Clear the version cache
	defer vdb.ClearCachedVersions()

	//initialize a missing key list
	missingKeys := []*statedb.CompositeKey{}

	//Revision numbers are needed for couchdb updates.
	//vdb.committedData.revisionNumbers is a cache of revision numbers based on ID
	//Document IDs and revision numbers may already be in the cache, but we need
	//a check here to verify that all Ids and revisions in the batch are represented
	//if the key is missing in the cache, then add the key to missingKeys
	//A bulk read will then add the missing revisions to the cache
	namespaces := batch.GetUpdatedNamespaces()
	for _, ns := range namespaces {
		updates := batch.GetUpdates(ns)
		for k := range updates {
			compositeKey := statedb.CompositeKey{Namespace: ns, Key: k}

			//check the cache to see if the key is missing
			_, keyFound := vdb.committedData.revisionNumbers[compositeKey]
			if !keyFound {

				//Add the key to the missing key list
				missingKeys = append(missingKeys, &compositeKey)

			}
		}
	}

	//only attempt to load missing keys if missing keys are detected
	if len(missingKeys) > 0 {

		logger.Debugf("Retrieving keys with unknown revision numbers, keys= %s", printCompositeKeys(missingKeys))

		vdb.LoadCommittedVersions(missingKeys)

	}

	batchUpdateDocs := []*couchdb.CouchDoc{}

	for _, ns := range namespaces {
		updates := batch.GetUpdates(ns)
		for k, vv := range updates {
			compositeKey := constructCompositeKey(ns, k)
			logger.Debugf("Channel [%s]: Applying key=[%#v]", vdb.dbName, compositeKey)

			//Create a document structure
			couchDoc := &couchdb.CouchDoc{}

			//retrieve the couchdb revision from the cache
			//Documents that do not exist in couchdb will not have revision numbers and will
			//exist in the cache with a revision value of nil
			revision := vdb.committedData.revisionNumbers[statedb.CompositeKey{Namespace: ns, Key: k}]

			if vv.Value == nil {

				//this is a deleted record.  Set the _deleted property to true
				couchDoc.JSONValue = addCouchDBFieldsToValue(string(compositeKey), revision, nil, ns, vv.Version, true)

			} else {

				if couchdb.IsJSON(string(vv.Value)) {
					// Handle as json
					couchDoc.JSONValue = addCouchDBFieldsToValue(string(compositeKey), revision, vv.Value, ns, vv.Version, false)

				} else {

					attachment := &couchdb.Attachment{}
					attachment.AttachmentBytes = vv.Value
					attachment.ContentType = "application/octet-stream"
					attachment.Name = binaryWrapper
					attachments := append([]*couchdb.Attachment{}, attachment)

					couchDoc.Attachments = attachments
					couchDoc.JSONValue = addCouchDBFieldsToValue(string(compositeKey), revision, nil, ns, vv.Version, false)

				}
			}

			//Add the document to the batch update
			batchUpdateDocs = append(batchUpdateDocs, couchDoc)

		}
	}

	if len(batchUpdateDocs) > 0 {

		batchUpdateResp, err := vdb.db.BatchUpdateDocuments(batchUpdateDocs)
		if err != nil {
			return err
		}

		for _, respDoc := range batchUpdateResp {
			if respDoc.Ok != true {

				errorString := fmt.Sprintf("Error occurred while saving document ID = %v  Error: %s  Reason: %s\n",
					respDoc.ID, respDoc.Error, respDoc.Reason)

				logger.Errorf(errorString)

				//TODO - FAB-2709 will enhance retry logic across the board.  This section dealing with error
				//conditions and returns will need to be revisited

				return fmt.Errorf(errorString)
			}
		}

	}

	// Record a savepoint at a given height
	err := vdb.recordSavepoint(height)
	if err != nil {
		logger.Errorf("Error during recordSavepoint: %s\n", err.Error())
		return err
	}

	return nil
}

//printCompositeKeys is a convenience method to print readable log entries for arrays of pointers
//to composite keys
func printCompositeKeys(keyPointers []*statedb.CompositeKey) string {

	returnKeys := []string{}

	for _, keyPointer := range keyPointers {

		returnKeys = append(returnKeys, "["+keyPointer.Namespace+","+keyPointer.Key+"]")
	}

	return strings.Join(returnKeys, ",")

}

// Same as LoadCommittedVersions except that it also loads values
//LoadCommittedValues populates committedVersions and revisionNumbers
func (vdb *VersionedDB) LoadCommittedValues(keys []*statedb.CompositeKey) {

	//initialize version and revision maps
	versionMap := vdb.committedData.committedVersions
	revMap := vdb.committedData.revisionNumbers
	valMap := vdb.committedData.committedValues

	keymap := []string{}
	for _, key := range keys {

		//create composite key for couchdb
		compositeDBKey := constructCompositeKey(key.Namespace, key.Key)
		//add the composite key to the list of required keys
		keymap = append(keymap, string(compositeDBKey))

		compositeKey := statedb.CompositeKey{Namespace: key.Namespace, Key: key.Key}

		//initialize empty values for each key
		versionMap[compositeKey] = nil
		revMap[compositeKey] = ""

	}

	docs, _ := vdb.db.BatchRetrieve(keymap, true)

	for _, doc := range docs {

		if len(doc.Version) != 0 {

			ns, key := splitCompositeKey([]byte(doc.ID))
			compositeKey := statedb.CompositeKey{Namespace: ns, Key: key}

			versionMap[compositeKey] = createVersionFromString(doc.Version)
			revMap[compositeKey] = doc.Rev

			var val []byte
			doc.Doc.UnmarshalJSON(val)
			valMap[compositeKey] = val

		}
	}
}

//LoadCommittedVersions populates committedVersions and revisionNumbers
func (vdb *VersionedDB) LoadCommittedVersions(keys []*statedb.CompositeKey) {

	//initialize version and revision maps
	versionMap := vdb.committedData.committedVersions
	revMap := vdb.committedData.revisionNumbers

	keymap := []string{}
	for _, key := range keys {

		//create composite key for couchdb
		compositeDBKey := constructCompositeKey(key.Namespace, key.Key)
		//add the composite key to the list of required keys
		keymap = append(keymap, string(compositeDBKey))

		compositeKey := statedb.CompositeKey{Namespace: key.Namespace, Key: key.Key}

		//initialize empty values for each key
		versionMap[compositeKey] = nil
		revMap[compositeKey] = ""

	}

	idVersions, _ := vdb.db.BatchRetrieve(keymap, false)

	for _, idVersion := range idVersions {

		if len(idVersion.Version) != 0 {

			ns, key := splitCompositeKey([]byte(idVersion.ID))
			compositeKey := statedb.CompositeKey{Namespace: ns, Key: key}

			versionMap[compositeKey] = createVersionFromString(idVersion.Version)
			revMap[compositeKey] = idVersion.Rev

		}
	}

}

func createVersionFromString(encodedVersion string) *version.Height {

	versionArray := strings.Split(fmt.Sprintf("%s", encodedVersion), ":")

	//convert the blockNum from String to unsigned int
	blockNum, _ := strconv.ParseUint(versionArray[0], 10, 64)

	//convert the txNum from String to unsigned int
	txNum, _ := strconv.ParseUint(versionArray[1], 10, 64)

	return version.NewHeight(blockNum, txNum)

}

//ClearCachedVersions clears committedVersions and revisionNumbers
func (vdb *VersionedDB) ClearCachedVersions() {

	versionMap := make(map[statedb.CompositeKey]*version.Height)
	revMap := make(map[statedb.CompositeKey]string)
	valMap := make(map[statedb.CompositeKey][]byte)

	vdb.committedData = &CommittedVersions{committedVersions: versionMap, revisionNumbers: revMap, committedValues: valMap}

}

//addCouchDBFieldsToValue adds keys to the CouchDoc.JSON value for the following items:
//_id - couchdb document ID, need for all couchdb batch operations
//_rev - couchdb document revision, needed for updating or deleting existing documents
//version - ledger version
//chaincodeID - chain code ID
//_deleted - flag using in batch operations for deleting a couchdb document
//The return value is the CouchDoc.JSONValue with the additional required CouchDB fields
func addCouchDBFieldsToValue(id, revision string, value []byte, chaincodeID string, version *version.Height, deleted bool) []byte {

	//create a version mapping
	jsonMap := map[string]interface{}{"version": fmt.Sprintf("%v:%v", version.BlockNum, version.TxNum)}

	//add the ID
	jsonMap["_id"] = id

	//add the revision
	if revision != "" {
		jsonMap["_rev"] = revision
	}

	//If this record is to be deleted, set the "_deleted" property to true
	if deleted {
		jsonMap["_deleted"] = true

	} else {

		//add the chaincodeID
		jsonMap["chaincodeid"] = chaincodeID

		//Add the wrapped data if the value is not null
		if value != nil {

			//create a new genericMap
			rawJSON := (*json.RawMessage)(&value)

			//add the rawJSON to the map
			jsonMap[dataWrapper] = rawJSON

		}

	}

	//The returnJSON is the CouchDoc.JSONValue, the additional fields will be
	//needed by CouchDB
	returnJSON, _ := json.Marshal(jsonMap)

	return returnJSON

}

// Savepoint docid (key) for couchdb
const savepointDocID = "statedb_savepoint"

// Savepoint data for couchdb
type couchSavepointData struct {
	BlockNum  uint64 `json:"BlockNum"`
	TxNum     uint64 `json:"TxNum"`
	UpdateSeq string `json:"UpdateSeq"`
}

// recordSavepoint Record a savepoint in statedb.
// Couch parallelizes writes in cluster or sharded setup and ordering is not guaranteed.
// Hence we need to fence the savepoint with sync. So ensure_full_commit is called before
// savepoint to ensure all block writes are flushed. Savepoint itself does not need to be flushed,
// it will get flushed with next block if not yet committed.
func (vdb *VersionedDB) recordSavepoint(height *version.Height) error {
	var err error
	var savepointDoc couchSavepointData
	// ensure full commit to flush all changes until now to disk
	dbResponse, err := vdb.db.EnsureFullCommit()
	if err != nil || dbResponse.Ok != true {
		logger.Errorf("Failed to perform full commit\n")
		return errors.New("Failed to perform full commit")
	}

	// construct savepoint document
	// UpdateSeq would be useful if we want to get all db changes since a logical savepoint
	dbInfo, _, err := vdb.db.GetDatabaseInfo()
	if err != nil {
		logger.Errorf("Failed to get DB info %s\n", err.Error())
		return err
	}
	savepointDoc.BlockNum = height.BlockNum
	savepointDoc.TxNum = height.TxNum
	savepointDoc.UpdateSeq = dbInfo.UpdateSeq

	savepointDocJSON, err := json.Marshal(savepointDoc)
	if err != nil {
		logger.Errorf("Failed to create savepoint data %s\n", err.Error())
		return err
	}

	// SaveDoc using couchdb client and use JSON format
	_, err = vdb.db.SaveDoc(savepointDocID, "", &couchdb.CouchDoc{JSONValue: savepointDocJSON, Attachments: nil})
	if err != nil {
		logger.Errorf("Failed to save the savepoint to DB %s\n", err.Error())
		return err
	}

	return nil
}

// GetLatestSavePoint implements method in VersionedDB interface
func (vdb *VersionedDB) GetLatestSavePoint() (*version.Height, error) {

	var err error
	couchDoc, _, err := vdb.db.ReadDoc(savepointDocID)
	if err != nil {
		logger.Errorf("Failed to read savepoint data %s\n", err.Error())
		return nil, err
	}

	// ReadDoc() not found (404) will result in nil response, in these cases return height nil
	if couchDoc == nil || couchDoc.JSONValue == nil {
		return nil, nil
	}

	savepointDoc := &couchSavepointData{}
	err = json.Unmarshal(couchDoc.JSONValue, &savepointDoc)
	if err != nil {
		logger.Errorf("Failed to unmarshal savepoint data %s\n", err.Error())
		return nil, err
	}

	return &version.Height{BlockNum: savepointDoc.BlockNum, TxNum: savepointDoc.TxNum}, nil
}

func constructCompositeKey(ns string, key string) []byte {
	compositeKey := []byte(ns)
	compositeKey = append(compositeKey, compositeKeySep...)
	compositeKey = append(compositeKey, []byte(key)...)
	return compositeKey
}

func splitCompositeKey(compositeKey []byte) (string, string) {
	split := bytes.SplitN(compositeKey, compositeKeySep, 2)
	return string(split[0]), string(split[1])
}

type kvScanner struct {
	cursor    int
	namespace string
	results   []couchdb.QueryResult
}

func newKVScanner(namespace string, queryResults []couchdb.QueryResult) *kvScanner {
	return &kvScanner{-1, namespace, queryResults}
}

func (scanner *kvScanner) Next() (statedb.QueryResult, error) {

	scanner.cursor++

	if scanner.cursor >= len(scanner.results) {
		return nil, nil
	}

	selectedKV := scanner.results[scanner.cursor]

	_, key := splitCompositeKey([]byte(selectedKV.ID))

	//remove the data wrapper and return the value and version
	returnValue, returnVersion := removeDataWrapper(selectedKV.Value, selectedKV.Attachments)

	return &statedb.VersionedKV{
		CompositeKey:   statedb.CompositeKey{Namespace: scanner.namespace, Key: key},
		VersionedValue: statedb.VersionedValue{Value: returnValue, Version: &returnVersion}}, nil
}

func (scanner *kvScanner) Close() {
	scanner = nil
}

type queryScanner struct {
	cursor  int
	results []couchdb.QueryResult
}

func newQueryScanner(queryResults []couchdb.QueryResult) *queryScanner {
	return &queryScanner{-1, queryResults}
}

func (scanner *queryScanner) Next() (statedb.QueryResult, error) {

	scanner.cursor++

	if scanner.cursor >= len(scanner.results) {
		return nil, nil
	}

	selectedResultRecord := scanner.results[scanner.cursor]

	namespace, key := splitCompositeKey([]byte(selectedResultRecord.ID))

	//remove the data wrapper and return the value and version
	returnValue, returnVersion := removeDataWrapper(selectedResultRecord.Value, selectedResultRecord.Attachments)

	return &statedb.VersionedKV{
		CompositeKey:   statedb.CompositeKey{Namespace: namespace, Key: key},
		VersionedValue: statedb.VersionedValue{Value: returnValue, Version: &returnVersion}}, nil
}

func (scanner *queryScanner) Close() {
	scanner = nil
}
