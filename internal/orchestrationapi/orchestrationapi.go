/*******************************************************************************
 * Copyright 2019-2020 Samsung Electronics All Rights Reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 *******************************************************************************/

package orchestrationapi

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lf-edge/edge-home-orchestration-go/internal/common/commandvalidator"
	"github.com/lf-edge/edge-home-orchestration-go/internal/common/networkhelper"
	"github.com/lf-edge/edge-home-orchestration-go/internal/common/requestervalidator"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/configuremgr"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/discoverymgr"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/scoringmgr"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/securemgr/verifier"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/servicemgr"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/servicemgr/notification"
	"github.com/lf-edge/edge-home-orchestration-go/internal/controller/storagemgr"
	"github.com/lf-edge/edge-home-orchestration-go/internal/db/bolt/common"
	sysDB "github.com/lf-edge/edge-home-orchestration-go/internal/db/bolt/system"
	dbhelper "github.com/lf-edge/edge-home-orchestration-go/internal/db/helper"
	"github.com/lf-edge/edge-home-orchestration-go/internal/restinterface/client"
	"github.com/robfig/cron"
)

type orcheImpl struct {
	Ready bool

	verifierIns     verifier.VerifierConf
	serviceIns      servicemgr.ServiceMgr
	scoringIns      scoringmgr.Scoring
	discoverIns     discoverymgr.Discovery
	watcher         configuremgr.Watcher
	notificationIns notification.Notification
	storageIns      storagemgr.Storage
	networkhelper   networkhelper.Network
	clientAPI       client.Clienter
}

type deviceInfo struct {
	id       string
	endpoint string
	score    float64
	resource map[string]interface{}
	execType string
}

type forwardNotification struct {
	targetURL string
	serviceId string
}

type orcheClient struct {
	appName   string
	args      []string
	notiChan  chan string
	endSignal chan bool
	forward   forwardNotification
}

// RequestServiceInfo struct
type RequestServiceInfo struct {
	ExecutionType string
	ExeCmd        []string
	ExeOption     map[string]interface{}
}

// ReqeustService struct
type ReqeustService struct {
	SelfSelection    bool
	ServiceName      string
	ServiceRequester string
	ServiceInfo      []RequestServiceInfo
	// TODO add status callback
}

// TargetInfo struct
type TargetInfo struct {
	ExecutionType string
	Target        string
}

// ResponseService struct
type ResponseService struct {
	Message          string
	ServiceName      string
	RemoteTargetInfo TargetInfo
}

const (
	// ErrorNone is key no error
	ErrorNone = "ERROR_NONE"
	// InvalidParameter is key for invalid parameter
	InvalidParameter = "INVALID_PARAMETER"
	//ServiceNotFound is key for service not found
	ServiceNotFound = "SERVICE_NOT_FOUND"
	//InternalServerError is key for internal server error
	InternalServerError = "INTERNAL_SERVER_ERROR"
	// NotAllowedCommand is key for not allowed command
	NotAllowedCommand = "NOT_ALLOWED_COMMAND"
)

var (
	orchClientID int32 = -1
	orcheClients       = [1024]orcheClient{}

	sysDBExecutor sysDB.DBInterface

	helper dbhelper.MultipleBucketQuery

	clusterUUID string = ""

	forwardNotificationInfo = make(map[string]forwardNotification)
)

func init() {
	sysDBExecutor = sysDB.Query{}

	helper = dbhelper.GetInstance()
}

func (orcheEngine *orcheImpl) SetupServerCommunication() {
	response, err := orcheEngine.clientAPI.DoRegisterClusterWithServer()

	if err != nil {
		log.Println("[orchestrationapi] cannot register cluster with cloud server because of", err.Error())
		return
	}

	clusterUUID = response["uuid"].(string)
	orcheEngine.SetupCycles(response["benchmarkCycle"].(string), response["assignmentCycle"].(string))
}

func (orcheEngine *orcheImpl) SetupCycles(benchmarkCycleExpression string, assignmentCycleExpression string) {
	c := cron.New()
	c.AddFunc(assignmentCycleExpression, func() {
		log.Println("Triggered checkup")
		orcheEngine.CloudCheckup()
	})
	c.AddFunc(benchmarkCycleExpression, func() {
		log.Println("Triggered Benchmarks update")
		orcheEngine.CloudUpdateBenchmarks()
	})
	c.Start()
	log.Printf("[orchestrationapi] assignment cron cycle: %s, entries:", assignmentCycleExpression)
	log.Printf("[orchestrationapi] benchmarks cron cycle: %s, entries:", benchmarkCycleExpression)
	for _, e := range c.Entries() {
		log.Printf("%+v", e)
	}
}

func (orcheEngine *orcheImpl) RequestServiceCloud(serviceName string, args []string, requesterId string, forward forwardNotification) {
	if !orcheEngine.Ready {
		log.Printf("[%s] orcheEngine not ready", "RequestServiceCloud")
		return
	}

	serviceRequest := map[string]interface{}{
		"clusterId": clusterUUID,
		"deviceId":  requesterId,
		"name":      serviceName,
		"command":   args,
	}

	executionUUID, err := orcheEngine.clientAPI.DoExecuteCloudScheduler(serviceRequest)
	if err != nil {
		log.Println("[orchestrationapi] cannot request service from server because of", err.Error())
		return
	}

	log.Println(executionUUID)
	forwardNotificationInfo[executionUUID] = forward
}

func (orcheEngine *orcheImpl) CloudUpdateBenchmarks() (err error) {
	if !orcheEngine.Ready {
		log.Printf("[%s] orcheEngine not ready", "CloudUpdateBenchmarks")
		return
	}

	candidates, err := orcheEngine.getCandidate("", []string{"container"})

	if err != nil {
		log.Printf("[%s] error getting candidate %s", "CloudUpdateBenchmarks", err.Error())
		return err
	}

	deviceResources := orcheEngine.gatherDevicesResource(candidates, true)
	if len(deviceResources) <= 0 {
		return errors.New("no suitable device found")
	}

	benchmarks := make(map[string][]interface{})
	log.Printf("[RequestService] CloudCheckup")
	for index, candidate := range deviceResources {
		for key, value := range candidate.resource {
			resource := map[string]interface{}{
				"type":  key,
				"value": value,
			}
			benchmarks[candidate.id] = append(benchmarks[candidate.id], resource)
		}
		log.Printf("[%d] Id       : %v", index, candidate.id)
		log.Printf("[%d] Resource : %v", index, candidate.resource)
		log.Printf("[%d] Score : %v", index, candidate.score)
		log.Printf("")
	}

	requestBody := map[string]interface{}{
		"clusterId":  clusterUUID,
		"benchmarks": benchmarks,
	}

	response, err := orcheEngine.clientAPI.DoServerBenchmarksUpdate(requestBody)
	if err != nil {
		log.Println("[orchestrationapi] cannot checkup on server because of", err.Error())
		return
	}

	log.Println(response, "CloudUpdateBenchmarks")
	return nil
}

func (orcheEngine *orcheImpl) CloudCheckup() (err error) {
	if !orcheEngine.Ready {
		log.Printf("[%s] orcheEngine not ready", "CloudCheckup")
		return
	}

	candidates, err := orcheEngine.getCandidate("", []string{"container"})

	if err != nil {
		log.Printf("[%s] error getting candidate %s", "CloudCheckup", err.Error())
		return err
	}

	requestBody := map[string]interface{}{
		"clusterId": clusterUUID,
	}

	response, err := orcheEngine.clientAPI.DoServerCheckup(requestBody)
	if err != nil {
		log.Println("[orchestrationapi] cannot checkup on server because of", err.Error())
		return
	}

	for _, serviceInterface := range response["services"].([]interface{}) {
		log.Printf("%v", serviceInterface)
		service, _ := serviceInterface.(map[string]interface{})

		//TODO: call finished endpoint on service done
		atomic.AddInt32(&orchClientID, 1)
		handle := int(orchClientID)
		forward := forwardNotification{"CLOUD", service["uuid"].(string)}
		serviceClient := addServiceClient(handle, service["name"].(string), forward)
		go serviceClient.listenNotify(orcheEngine.notificationIns, orcheEngine.clientAPI)

		var endpoint string
		for _, device := range candidates {
			if device.Id == service["device"].(string) {
				if len(device.Endpoint) > 0 {
					endpoint = device.Endpoint[0]
				}
			}
		}

		if endpoint == "" {
			log.Printf("[orchestrationapi] can't execute service %s, no device found with id %s\n", service["uuid"].(string), service["device"].(string))
		} else {
			var commands []string
			for _, command := range service["command"].([]interface{}) {
				commands = append(commands, command.(string))
			}
			orcheEngine.executeApp(
				endpoint,
				service["name"].(string),
				service["uuid"].(string),
				commands,
				serviceClient.notiChan,
			)
		}
	}

	// loop over notifications and forward them to original requesters
	for _, notificationInterface := range response["notifications"].([]interface{}) {
		log.Printf("%v", notificationInterface)
		notification, _ := notificationInterface.(map[string]interface{})
		serviceUuid := notification["uuid"].(string)
		forward := forwardNotificationInfo[serviceUuid]

		//TODO: get status and pass it instead of hardcoded Success
		log.Printf("[orchestrationapi] service status changed [uuid:%s][status:%s]\n", serviceUuid, "Success")
		//forward notification to original requester
		if forward.targetURL != "" {
			serviceId, err := strconv.ParseFloat(forward.serviceId, 64)
			if err != nil {
				log.Printf("[orchestrationapi] cannot checkup on server while mapping notification serviceId: %s\n", forward.serviceId, err.Error())
			}
			orcheEngine.notificationIns.InvokeNotification(forward.targetURL, serviceId, "Success")
		}

		delete(forwardNotificationInfo, serviceUuid)
	}

	return nil
}

// RequestServiceCenterlized handles service request (ex. offloading) from other workers on the network
func (orcheEngine *orcheImpl) RequestServiceCenterlized(appInfo map[string]interface{}) {

	serviceName := appInfo["ServiceName"].(string)

	if !orcheEngine.Ready {
		log.Printf("[%s] orcheEngine not ready", "RequestServiceCenterlized")
		return
	}

	executionTypes := make([]string, 0)

	args := make([]string, 0)
	for _, arg := range appInfo["UserArgs"].([]interface{}) {
		args = append(args, arg.(string))
	}
	executionTypes = append(executionTypes, args[len(args)-1])

	forward := forwardNotification{appInfo["NotificationTargetURL"].(string), fmt.Sprintf("%f", appInfo["ServiceID"].(float64))}

	device, err := orcheEngine.scheduleService(serviceName, executionTypes, "device", true)
	if err != nil {
		orcheEngine.RequestServiceCloud(serviceName, args, appInfo["Requester"].(string), forward)
		return
	}

	atomic.AddInt32(&orchClientID, 1)

	handle := int(orchClientID)

	serviceClient := addServiceClient(handle, serviceName, forward)
	go serviceClient.listenNotify(orcheEngine.notificationIns, orcheEngine.clientAPI)

	orcheEngine.executeApp(
		device.endpoint,
		serviceName,
		appInfo["Requester"].(string),
		args,
		serviceClient.notiChan,
	)
}

// RequestService handles service request (ex. offloading) from service application
func (orcheEngine *orcheImpl) RequestService(serviceInfo ReqeustService) ResponseService {
	log.Printf("[RequestService] %v: %v\n", serviceInfo.ServiceName, serviceInfo.ServiceInfo)

	if !orcheEngine.Ready {
		return ResponseService{
			Message:          InternalServerError,
			ServiceName:      serviceInfo.ServiceName,
			RemoteTargetInfo: TargetInfo{},
		}
	}

	atomic.AddInt32(&orchClientID, 1)

	handle := int(orchClientID)

	serviceClient := addServiceClient(handle, serviceInfo.ServiceName, forwardNotification{})
	go serviceClient.listenNotify(orcheEngine.notificationIns, orcheEngine.clientAPI)

	executionTypes := make([]string, 0)
	var scoringType string
	for _, info := range serviceInfo.ServiceInfo {
		executionTypes = append(executionTypes, info.ExecutionType)
		scoringType, _ = info.ExeOption["scoringType"].(string)
	}

	device, err := orcheEngine.scheduleService(serviceInfo.ServiceName, executionTypes, scoringType, serviceInfo.SelfSelection)
	if true || err != nil {
		args, err := getExecCmds("container", serviceInfo.ServiceInfo)
		if err != nil {
			log.Println(err.Error())
			return ResponseService{
				Message:          err.Error(),
				ServiceName:      serviceInfo.ServiceName,
				RemoteTargetInfo: TargetInfo{},
			}
		}
		args = append(args, "container")
		orcheEngine.RequestServiceCloud(serviceInfo.ServiceName, args, serviceInfo.ServiceRequester, forwardNotification{})
		return ResponseService{
			Message:          err.Error(),
			ServiceName:      serviceInfo.ServiceName,
			RemoteTargetInfo: TargetInfo{},
		}
	}

	args, err := getExecCmds(device.execType, serviceInfo.ServiceInfo)
	if err != nil {
		log.Println(err.Error())
		return ResponseService{
			Message:          err.Error(),
			ServiceName:      serviceInfo.ServiceName,
			RemoteTargetInfo: TargetInfo{},
		}
	}
	args = append(args, device.execType)

	localhosts, err := orcheEngine.networkhelper.GetIPs()
	if err != nil {
		log.Println("[orchestrationapi] localhost ip gettering fail. maybe skipped localhost")
	}

	if common.HasElem(localhosts, device.endpoint) {
		validator := commandvalidator.CommandValidator{}
		for _, info := range serviceInfo.ServiceInfo {
			if info.ExecutionType == "native" || info.ExecutionType == "android" {
				if err := validator.CheckCommand(serviceInfo.ServiceName, info.ExeCmd); err != nil {
					log.Println(err.Error())
					return ResponseService{
						Message:          err.Error(),
						ServiceName:      serviceInfo.ServiceName,
						RemoteTargetInfo: TargetInfo{},
					}
				}
			}
		}

		vRequester := requestervalidator.RequesterValidator{}
		if err := vRequester.CheckRequester(serviceInfo.ServiceName, serviceInfo.ServiceRequester); err != nil &&
			(device.execType == "native" || device.execType == "android") {
			log.Println(err.Error())
			return ResponseService{
				Message:          err.Error(),
				ServiceName:      serviceInfo.ServiceName,
				RemoteTargetInfo: TargetInfo{},
			}
		}
	}

	orcheEngine.executeApp(
		device.endpoint,
		serviceInfo.ServiceName,
		serviceInfo.ServiceRequester,
		args,
		serviceClient.notiChan,
	)

	return ResponseService{
		Message:     ErrorNone,
		ServiceName: serviceInfo.ServiceName,
		RemoteTargetInfo: TargetInfo{
			ExecutionType: device.execType,
			Target:        device.endpoint,
		},
	}
}

func (orcheEngine *orcheImpl) scheduleService(serviceName string, executionTypes []string, scoringType string, selfSelection bool) (device deviceInfo, err error) {
	candidates, err := orcheEngine.getCandidate(serviceName, executionTypes)

	log.Printf("[RequestService] getCandidate")
	for index, candidate := range candidates {
		log.Printf("[%d] Id       : %v", index, candidate.Id)
		log.Printf("[%d] ExecType : %v", index, candidate.ExecType)
		log.Printf("[%d] Endpoint : %v", index, candidate.Endpoint)
		log.Printf("")
	}

	if err != nil {
		log.Printf("[%s] error getting candidate %s", "RequestServiceCenterlized", err.Error())
		return deviceInfo{}, err
	}

	var deviceScores []deviceInfo

	if scoringType == "resource" {
		deviceResources := orcheEngine.gatherDevicesResource(candidates, selfSelection)
		if len(deviceResources) <= 0 {
			return deviceInfo{}, errors.New(ServiceNotFound)
		}
		for i, dev := range deviceResources {
			deviceResources[i].score, _ = orcheEngine.GetScoreWithResource(dev.resource)
		}
		deviceScores = sortByScore(deviceResources)
	} else {
		deviceScores = sortByScore(orcheEngine.gatherDevicesScore(candidates, selfSelection))
	}

	if len(deviceScores) <= 0 || deviceScores[0].score == scoringmgr.InvalidScore {
		log.Printf("[%s] %s couldn't find a device with a valid score", "RequestServiceCenterlized", ServiceNotFound)
		return deviceInfo{}, errors.New(ServiceNotFound)
	}

	log.Println("[orchestrationapi] ", deviceScores)

	return deviceScores[0], nil
}

func getExecCmds(execType string, requestServiceInfos []RequestServiceInfo) ([]string, error) {
	for _, requestServiceInfo := range requestServiceInfos {
		if execType == requestServiceInfo.ExecutionType {
			return requestServiceInfo.ExeCmd, nil
		}
	}

	return nil, errors.New("not found")
}

func (orcheEngine orcheImpl) getCandidate(appName string, execType []string) (deviceList []dbhelper.ExecutionCandidate, err error) {
	return helper.GetDeviceInfoWithService(appName, execType)
}

func (orcheEngine orcheImpl) gatherDevicesScore(candidates []dbhelper.ExecutionCandidate, selfSelection bool) (deviceScores []deviceInfo) {
	count := len(candidates)
	if !selfSelection {
		count--
	}
	scores := make(chan deviceInfo, count)

	info, err := sysDBExecutor.Get(sysDB.ID)
	if err != nil {
		log.Println("[orchestrationapi] localhost devid gettering fail")
		return
	}

	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(3 * time.Second)
		timeout <- true
	}()

	var wait sync.WaitGroup
	wait.Add(1)
	index := 0
	go func() {
		defer wait.Done()
		for {
			select {
			case score := <-scores:
				deviceScores = append(deviceScores, score)
				if index++; count == index {
					return
				}
			case <-timeout:
				return
			}
		}
	}()

	localhosts, err := orcheEngine.networkhelper.GetIPs()
	if err != nil {
		log.Println("[orchestrationapi] localhost ip gettering fail. maybe skipped localhost")
	}

	for _, candidate := range candidates {
		go func(cand dbhelper.ExecutionCandidate) {
			var score float64
			var err error

			if len(cand.Endpoint) == 0 {
				log.Println("[orchestrationapi] cannot getting score, cause by ip list is empty")
				scores <- deviceInfo{endpoint: "", score: float64(0.0), id: cand.Id}
				return
			}

			if isLocalhost(cand.Endpoint, localhosts) {
				if !selfSelection {
					return
				}
				score, err = orcheEngine.GetScore(info.Value)
			} else {
				score, err = orcheEngine.clientAPI.DoGetScoreRemoteDevice(info.Value, cand.Endpoint[0])
			}

			if err != nil {
				log.Println("[orchestrationapi] cannot getting score from :", cand.Endpoint[0], "cause by", err.Error())
				scores <- deviceInfo{endpoint: cand.Endpoint[0], score: float64(0.0), id: cand.Id}
				return
			}
			log.Printf("[orchestrationapi] deviceScore")
			log.Printf("candidate Id       : %v", cand.Id)
			log.Printf("candidate ExecType : %v", cand.ExecType)
			log.Printf("candidate Endpoint : %v", cand.Endpoint[0])
			log.Printf("candidate score    : %v", score)
			scores <- deviceInfo{endpoint: cand.Endpoint[0], score: score, id: cand.Id, execType: cand.ExecType}
		}(candidate)
	}

	wait.Wait()

	return
}

// gatherDevicesResource gathers resource values from edge devices
func (orcheEngine orcheImpl) gatherDevicesResource(candidates []dbhelper.ExecutionCandidate, selfSelection bool) (deviceResources []deviceInfo) {
	count := len(candidates)
	if !selfSelection {
		count--
	}
	resources := make(chan deviceInfo, count)

	info, err := sysDBExecutor.Get(sysDB.ID)
	if err != nil {
		log.Println("[orchestrationapi] localhost devid gettering fail")
		return
	}

	timeout := make(chan bool, 1)
	go func() {
		time.Sleep(3 * time.Second)
		timeout <- true
	}()

	var wait sync.WaitGroup
	wait.Add(1)
	index := 0
	go func() {
		defer wait.Done()
		for {
			select {
			case resource := <-resources:
				deviceResources = append(deviceResources, resource)
				if index++; count == index {
					return
				}
			case <-timeout:
				return
			}
		}
	}()

	localhosts, err := orcheEngine.networkhelper.GetIPs()
	if err != nil {
		log.Println("[orchestrationapi] localhost ip gettering fail. maybe skipped localhost")
	}

	for _, candidate := range candidates {
		go func(cand dbhelper.ExecutionCandidate) {
			var resource map[string]interface{}
			var err error

			if len(cand.Endpoint) == 0 {
				log.Println("[orchestrationapi] cannot getting score, cause by ip list is empty")
				resources <- deviceInfo{endpoint: "", resource: resource, id: cand.Id, execType: cand.ExecType}
				return
			}

			if isLocalhost(cand.Endpoint, localhosts) {
				if !selfSelection {
					return
				}
				resource, err = orcheEngine.GetResource("Cloud")
			} else {
				resource, err = orcheEngine.clientAPI.DoGetResourceRemoteDevice(info.Value, cand.Endpoint[0])

				headResource, _ := orcheEngine.GetResource("Cloud")
				resource["rtt"] = headResource["rtt"].(float64) + resource["rtt"].(float64)
			}

			if err != nil {
				log.Println("[orchestrationapi] cannot getting msgs from :", cand.Endpoint[0], "cause by", err.Error())
				resources <- deviceInfo{endpoint: cand.Endpoint[0], resource: resource, id: cand.Id, execType: cand.ExecType}
				return
			}
			log.Printf("[orchestrationapi] deviceResource")
			log.Printf("candidate Id       : %v", cand.Id)
			log.Printf("candidate ExecType : %v", cand.ExecType)
			log.Printf("candidate Endpoint : %v", cand.Endpoint[0])
			log.Printf("candidate resource : %v", resource)
			resources <- deviceInfo{endpoint: cand.Endpoint[0], resource: resource, id: cand.Id, execType: cand.ExecType}
		}(candidate)
	}

	wait.Wait()

	return
}

func (orcheEngine orcheImpl) executeApp(endpoint, serviceName, requester string, args []string, notiChan chan string) {
	ifArgs := make([]interface{}, len(args))
	for i, v := range args {
		ifArgs[i] = v
	}

	orcheEngine.serviceIns.Execute(endpoint, serviceName, requester, ifArgs, notiChan)
}

func (client *orcheClient) listenNotify(notificationIns notification.Notification, clientAPI client.Clienter) {
	select {
	case str := <-client.notiChan:
		log.Printf("[orchestrationapi] service status changed [appNames:%s][status:%s]\n", client.appName, str)
		//forward notification to original requester
		if client.forward.targetURL == "CLOUD" {
			requestBody := map[string]interface{}{
				"clusterId": clusterUUID,
				"uuid":      client.forward.serviceId,
			}
			_, err := clientAPI.DoServerServiceNotification(requestBody)
			if err != nil {
				log.Printf("[orchestrationapi] error sending notification to cloud server: %s\n", client.forward.serviceId, err.Error())
			}
		} else if client.forward.targetURL != "" {
			serviceId, err := strconv.ParseFloat(client.forward.serviceId, 64)
			if err != nil {
				log.Printf("[orchestrationapi] local notification serviceId: %s\n", client.forward.serviceId, err.Error())
			}
			notificationIns.InvokeNotification(client.forward.targetURL, serviceId, str)
		}
	}
}

func isLocalhost(endpoints1, endpoints2 []string) bool {
	for _, endpoint1 := range endpoints1 {
		for _, endpoint2 := range endpoints2 {
			if endpoint1 == endpoint2 {
				return true
			}
		}
	}
	return false
}

func addServiceClient(clientID int, appName string, forward forwardNotification) (client *orcheClient) {
	// orcheClients[clientID].args = args
	orcheClients[clientID].appName = appName
	orcheClients[clientID].notiChan = make(chan string)
	orcheClients[clientID].forward = forward

	client = &orcheClients[clientID]
	return
}

func sortByScore(deviceScores []deviceInfo) []deviceInfo {
	sort.Slice(deviceScores, func(i, j int) bool {
		return deviceScores[i].score > deviceScores[j].score
	})

	return deviceScores
}
