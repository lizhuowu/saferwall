// Copyright 2018 Saferwall. All rights reserved.
// Use of this source code is governed by Apache v2 license
// license that can be found in the LICENSE file.

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"time"

	gonsq "github.com/nsqio/go-nsq"
	"github.com/saferwall/saferwall/internal/behavior"
	"github.com/saferwall/saferwall/internal/hasher"
	"github.com/saferwall/saferwall/internal/log"
	"github.com/saferwall/saferwall/internal/pubsub"
	"github.com/saferwall/saferwall/internal/pubsub/nsq"
	"github.com/saferwall/saferwall/internal/random"
	"github.com/saferwall/saferwall/internal/utils"
	"github.com/saferwall/saferwall/internal/vmmanager"
	goyara "github.com/saferwall/saferwall/internal/yara"
	micro "github.com/saferwall/saferwall/services"
	"github.com/saferwall/saferwall/services/config"
	pb "github.com/saferwall/saferwall/services/proto"
	"google.golang.org/protobuf/proto"

	"github.com/google/uuid"
)

var (
	errNotEnoughResources = errors.New("failed to find a free VM")
)

const (
	windows7  = "win-7"
	windows10 = "win-10"
)

// Config represents our application config.
type Config struct {
	LogLevel      string             `mapstructure:"log_level"`
	SharedVolume  string             `mapstructure:"shared_volume"`
	EnglishWords  string             `mapstructure:"english_words"`
	YaraRules     string             `mapstructure:"yara_rules"`
	BehaviorRules string             `mapstructure:"behavior_rules"`
	Agent         AgentCfg           `mapstructure:"agent"`
	VirtMgr       VirtManagerCfg     `mapstructure:"virt_manager"`
	Producer      config.ProducerCfg `mapstructure:"producer"`
	Consumer      config.ConsumerCfg `mapstructure:"consumer"`
	Sandbox       SandboxCfg         `mapstructure:"sandbox"`
}

// AgentCfg represents the guest agent config.
type AgentCfg struct {
	// Destination directory inside the guest where the agent is deployed.
	AgentDestDir string `mapstructure:"dest_dir"`
	// The sandbox binary components.
	PackageName string `mapstructure:"package_name"`
}

// VirtManagerCfg represents the virtualization manager config.
// For now, only libvirt server.
type VirtManagerCfg struct {
	Network      string `mapstructure:"network"`
	Address      string `mapstructure:"address"`
	Port         string `mapstructure:"port"`
	User         string `mapstructure:"user"`
	SSHKeyPath   string `mapstructure:"ssh_key_path"`
	SnapshotName string `mapstructure:"snapshot_name"`
}

// SandboxCfg represents the sandbox config.
type SandboxCfg struct {
	LogLevel  string `mapstructure:"log_level"`
	HidePaths string `mapstructure:"hide_paths"`
}

// Service represents the sandbox scan service. It adheres to the nsq.Handler
// interface. This allows us to define our own custom handlers for our messages.
// Think of these handlers much like you would an http handler.
type Service struct {
	cfg         Config
	logger      log.Logger
	pub         pubsub.Publisher
	sub         pubsub.Subscriber
	vms         []*VM
	vmm         vmmanager.VMManager
	randomizer  random.Ramdomizer
	hasher      hasher.Hasher
	yaraScanner goyara.Scanner
	bhvScanner  behavior.Scanner
	sandbox     []byte
}

func toJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// generateGUID returns a unique ID to identify a document.
func generateGUID() string {
	id := uuid.New()
	return id.String()
}

// New create a new sandbox service.
func New(cfg Config, logger log.Logger) (*Service, error) {
	var err error
	s := Service{}

	// Retrieve the list of active domains.
	logger.Infof("virt-manager config: %v, node IP: %v", cfg.VirtMgr, os.Getenv("NODE_IP"))
	conn, err := vmmanager.New(cfg.VirtMgr.Network, cfg.VirtMgr.Address,
		cfg.VirtMgr.Port, cfg.VirtMgr.User, cfg.VirtMgr.SSHKeyPath)
	if err != nil {
		return nil, err
	}
	dd, err := conn.Domains()
	if err != nil {
		return nil, err
	}

	// Only for debugging: revert the domains to their clean state.
	if false {
		for _, dom := range dd {
			logger.Infof("reverting %s to: %s", dom.Dom.Name, cfg.VirtMgr.SnapshotName)
			go func(dom vmmanager.Domain) {
				err = conn.Revert(dom.Dom, cfg.VirtMgr.SnapshotName)
				if err != nil {
					logger.Errorf("failed to revert the VM: %v", err)
				}
			}(dom)
		}
		time.Sleep(30 * time.Second)
	}

	// TODO what happens when len(vms) is 0.
	// Also, when we repair a broken VM, we want to refresh the list
	// of domains, a potential solution is to fire a thread that sync
	// the list of active domains every X minutes.
	var vms []*VM
	for _, d := range dd {
		vm := VM{
			ID:        d.Dom.ID,
			Name:      d.Dom.Name,
			IP:        d.IP,
			Snapshots: d.Snapshots,
			InUse:     false,
			IsHealthy: true,
			Dom:       d.Dom,
		}

		// Ping the server inside the VM and validate it is healthy.
		err = vm.ping()
		if err != nil {
			logger.Errorf("failed to ping domain %s: %v", vm.Name, err)
			continue
		}

		logger.Infof("%s VM (id: %v, ip: %s) running %s", vm.Name, vm.ID, vm.IP, vm.OS)
		vms = append(vms, &vm)
	}

	if len(vms) == 0 {
		return nil, errors.New("no VM is running")
	}

	// The number of concurrent workers have to match the number of
	// available virtual machines.
	s.sub, err = nsq.NewSubscriber(
		cfg.Consumer.Topic,
		cfg.Consumer.Channel,
		cfg.Consumer.Lookupds,
		len(vms),
		&s,
	)
	if err != nil {
		return nil, err
	}

	s.pub, err = nsq.NewPublisher(cfg.Producer.Nsqd)
	if err != nil {
		return nil, err
	}

	// Download the sandbox release package.
	zipPackageData, err := utils.ReadAll(cfg.Agent.PackageName)
	if err != nil {
		return nil, err
	}

	// Create a string randomizer.
	randomSvc, err := random.New(cfg.EnglishWords)
	if err != nil {
		return nil, err
	}

	// Create a new hasher.
	s.hasher = hasher.New(sha256.New())

	// Create a new yara scanner.
	s.yaraScanner, err = goyara.New(cfg.YaraRules)
	if err != nil {
		return nil, err
	}

	// Init the lua state.
	s.bhvScanner, err = behavior.New(cfg.BehaviorRules, logger)
	if err != nil {
		return nil, err
	}

	s.sandbox = zipPackageData
	s.vms = vms
	s.cfg = cfg
	s.logger = logger
	s.vmm = conn
	s.randomizer = randomSvc
	return &s, nil

}

// Start kicks in the service to start consuming events.
func (s *Service) Start() error {
	s.logger.Infof("start consuming from topic: %s ...", s.cfg.Consumer.Topic)
	return s.sub.Start()
}

// HandleMessage is the only requirement needed to fulfill the nsq.Handler.
func (s *Service) HandleMessage(m *gonsq.Message) error {
	if len(m.Body) == 0 {
		return errors.New("body is blank re-enqueue message")
	}

	ctx := context.Background()

	// Generate a unique ID for this behavior scan report.
	behaviorReportID := generateGUID()

	// Deserialize the scan config given by the user.
	fileScanCfg := config.FileScanCfg{}
	err := json.Unmarshal(m.Body, &fileScanCfg)
	if err != nil {
		s.logger.Errorf("failed un-marshalling json message body: %v", err)
		return err
	}

	sha256 := fileScanCfg.SHA256
	logger := s.logger.With(ctx, "sha256", sha256, "guid", behaviorReportID)
	logger.Infof("start processing sample with config: %#v", fileScanCfg.DynFileScanCfg)

	// Update the state of the job to `processing`.
	bhvDoc := make(map[string]interface{})
	now := time.Now().Unix()
	bhvDoc["sha256"] = sha256
	bhvDoc["timestamp"] = now
	bhvDoc["type"] = "behavior"
	bhvDoc["status"] = micro.FileScanProgressProcessing
	bhvDoc["doc"] = micro.DocMetadata{CreatedAt: now, LastUpdated: now, Version: 1}

	// Create also a new doc for api trace.
	apiTraceDoc := make(map[string]interface{})
	apiTraceDoc["type"] = "api-trace"
	apiTraceDoc["sha256"] = sha256
	apiTraceDocID := behaviorReportID + "::" + "apis"

	// Create a new doc for system events.
	sysEventsDoc := make(map[string]interface{})
	sysEventsDoc["type"] = "sys-events"
	sysEventsDoc["sha256"] = sha256
	sysEventsDocID := behaviorReportID + "::" + "events"

	payloads := []*pb.Message_Payload{
		{Key: behaviorReportID, Body: toJSON(bhvDoc), Kind: pb.Message_DBCREATE},
		{Key: apiTraceDocID, Body: toJSON(apiTraceDoc), Kind: pb.Message_DBCREATE},
		{Key: sysEventsDocID, Body: toJSON(sysEventsDoc), Kind: pb.Message_DBCREATE},
	}

	msg := &pb.Message{Sha256: sha256, Payload: payloads}
	out, err := proto.Marshal(msg)
	if err != nil {
		logger.Errorf("failed to marshal message: %v", err)
		return err
	}
	err = s.pub.Publish(ctx, s.cfg.Producer.Topic, out)
	if err != nil {
		logger.Errorf("failed to publish message: %v", err)
		return err
	}

	// Set default values for the scan config.
	if fileScanCfg.Timeout == 0 {
		fileScanCfg.Timeout = defaultFileScanTimeout
	}
	if fileScanCfg.DestPath == "" {
		randomFilename := s.randomizer.Random()
		fileScanCfg.DestPath = "%USERPROFILE%/Downloads/" + randomFilename + ".exe"
	}
	fileScanCfg.LogLevel = s.cfg.Sandbox.LogLevel
	fileScanCfg.HidePaths = s.cfg.Sandbox.HidePaths

	// Find a free VM to process this job.
	// Normally, we start as many concurrent worker as the number of VM we have, however
	// it's possible that clients requests a the same preferred OS multiple times.
	var vm *VM
	for i := 0; i < 3; i++ {
		vm = findFreeVM(s.vms, fileScanCfg.OS)
		if vm != nil {
			break
		}
		logger.Infof("no VM currently available that satisfies preferred OS: %s, sleep %s ...",
			fileScanCfg.OS, defaultFileScanTimeout*time.Second)
		time.Sleep(defaultFileScanTimeout * time.Second)
	}
	if vm == nil {
		return errNotEnoughResources
	}

	logger.Infof("VM [%s] with ID: %d was selected", vm.Name, vm.ID)
	logger = logger.With(ctx, "VM", vm.Name)

	// Perform the actual behavior scan.
	res, errDetonation := s.detonate(logger, vm, fileScanCfg)
	if errDetonation != nil {
		logger.Errorf("behavior analysis failed with: %v", errDetonation)
	} else {
		logger.Info("behavior analysis succeeded")
	}

	// Reverting the VM to a clean state at the end of the analysis
	// is safer than during the start of the analysis, as we instantly
	// stop the malware from running further.
	err = s.vmm.Revert(vm.Dom, s.cfg.VirtMgr.SnapshotName)
	if err != nil {
		logger.Errorf("failed to revert the VM: %v", err)

		// Mark the VM as non healthy so we can repair it.
		logger.Info("marking the VM as stale")
		vm.markStale()

	} else {
		// Free the VM for next job now, then continue on processing
		// sandbox results.
		logger.Info("freeing the VM")
		vm.free()
	}

	// Append this behavior report to the list of behavior reports for the file object.
	behaviorReport := make(map[string]interface{})
	behaviorReport["id"] = behaviorReportID
	behaviorReport["timestamp"] = now

	// Merge `scanConfig` into `behaviorReport`.
	var fileScanConfig map[string]interface{}
	jsonFileScanConfig := toJSON(res.ScanCfg)
	json.Unmarshal(jsonFileScanConfig, &fileScanConfig)
	for k, v := range fileScanConfig {
		behaviorReport[k] = v
	}
	behaviorReport["sandbox_ver"] = res.Environment.SandboxVersion

	// Create the `behavior` field in the file object.
	screenshotsCount := len(res.Screenshots) / 2
	defaultBehaviorReport := make(map[string]interface{})
	defaultBehaviorReport["capabilities"] = res.Capabilities
	defaultBehaviorReport["id"] = behaviorReportID
	defaultBehaviorReport["screenshots_count"] = screenshotsCount

	// If something went wrong during behavior analysis, we still want to
	// upload the results back to the backend.
	behaviorReportKey := sha256 + "/" + behaviorReportID + "/"
	agentLogKey := behaviorReportKey + "agent_log.json"
	sandboxLogKey := behaviorReportKey + "sandbox_log.json"
	apiTraceKey := behaviorReportKey + "api_trace.json"
	procTreeKey := behaviorReportKey + "proc_tree.json"
	payloads = []*pb.Message_Payload{
		{Key: apiTraceDocID, Path: "api_trace", Body: res.APITrace, Kind: pb.Message_DBUPDATE},
		{Key: sysEventsDocID, Path: "sys_events", Body: toJSON(res.Events), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "agent_log", Body: res.AgentLog, Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "sandbox_log", Body: res.SandboxLog, Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "proc_tree", Body: toJSON(res.ProcessTree), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "env", Body: toJSON(res.Environment), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "scan_cfg", Body: toJSON(res.ScanCfg), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "artifacts", Body: toJSON(res.Artifacts), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "capabilities", Body: toJSON(res.Capabilities), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "screenshots_count", Body: toJSON(screenshotsCount), Kind: pb.Message_DBUPDATE},
		{Key: behaviorReportID, Path: "status", Body: toJSON(micro.FileScanProgressFinished), Kind: pb.Message_DBUPDATE},
		{Key: agentLogKey, Body: res.AgentLog, Kind: pb.Message_UPLOAD},
		{Key: sandboxLogKey, Body: res.SandboxLog, Kind: pb.Message_UPLOAD},
		{Key: procTreeKey, Body: toJSON(res.ProcessTree), Kind: pb.Message_UPLOAD},
		{Key: apiTraceKey, Body: res.FullAPITrace, Kind: pb.Message_UPLOAD},
		// Add this behavior report to the list of behavior reports.
		{Key: sha256, Path: "behavior_scans." + behaviorReportID, Body: toJSON(behaviorReport),
			Kind: pb.Message_DBUPDATE},
		// These fields are duplicated to the `file` resource to avoid a join.
		{Key: sha256, Path: "default_behavior_report", Body: toJSON(defaultBehaviorReport),
			Kind: pb.Message_DBUPDATE},
	}

	// Screenshots are uploaded to file system storage like s3.
	for _, sc := range res.Screenshots {
		objKey := behaviorReportKey + "screenshots" + "/" + sc.Name
		payload := pb.Message_Payload{
			Key:  objKey,
			Body: sc.Content,
			Kind: pb.Message_UPLOAD,
		}
		payloads = append(payloads, &payload)
	}

	// Artifacts like memory dumps and dropped files are also uploaded to
	// file system storage like s3.
	for _, artifact := range res.Artifacts {
		objKey := behaviorReportKey + "artifacts" + "/" + artifact.Name
		payload := pb.Message_Payload{
			Key:  objKey,
			Body: artifact.Content,
			Kind: pb.Message_UPLOAD,
		}
		payloads = append(payloads, &payload)
	}

	// API Buffers are also uploaded to file system storage like s3.
	for _, apiBuff := range res.APIBuffers {
		objKey := behaviorReportKey + "api-buffers" + "/" + apiBuff.Name
		payload := pb.Message_Payload{
			Key:  objKey,
			Body: apiBuff.Content,
			Kind: pb.Message_UPLOAD,
		}
		payloads = append(payloads, &payload)
	}

	msg = &pb.Message{Sha256: sha256, Payload: payloads}
	out, err = proto.Marshal(msg)
	if err != nil {
		logger.Errorf("failed to marshal message: %v", err)
		return err
	}

	err = s.pub.Publish(ctx, s.cfg.Producer.Topic, out)
	if err != nil {
		logger.Errorf("failed to publish message: %v", err)
		return err
	}

	return nil
}
