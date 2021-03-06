/*
   Copyright 2015 Shlomi Noach, courtesy Booking.com

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

package orchestrator

import (
	"fmt"
	"github.com/outbrain/golib/log"
	"github.com/outbrain/orchestrator/config"
	"github.com/outbrain/orchestrator/inst"
	"github.com/outbrain/orchestrator/os"
	"github.com/pmylund/go-cache"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TopologyRecovery represents an entry in the topology_recovery table
type TopologyRecovery struct {
	TopologyRecoveryId     int64
	AnalysisEntry          inst.ReplicationAnalysis
	SuccessorKey           inst.InstanceKey
	IsActive               bool
	RecoveryStartTimestamp string
	RecoveryEndTimestamp   string
	ProcessingNodeHostname string
	ProcessingNodeToken    string
}

var emergencyReadTopologyInstanceMap = cache.New(time.Duration(config.Config.DiscoveryPollSeconds)*time.Second, time.Duration(config.Config.DiscoveryPollSeconds)*time.Second)

// InstancesByCountSlaves sorts instances by umber of slaves, descending
type InstancesByCountSlaves [](*inst.Instance)

func (this InstancesByCountSlaves) Len() int      { return len(this) }
func (this InstancesByCountSlaves) Swap(i, j int) { this[i], this[j] = this[j], this[i] }
func (this InstancesByCountSlaves) Less(i, j int) bool {
	if len(this[i].SlaveHosts) == len(this[j].SlaveHosts) {
		// Secondary sorting: prefer more advanced slaves
		return !this[i].ExecBinlogCoordinates.SmallerThan(&this[j].ExecBinlogCoordinates)
	}
	return len(this[i].SlaveHosts) < len(this[j].SlaveHosts)
}

// replaceCommandPlaceholders replaxces agreed-upon placeholders with analysis data
func replaceCommandPlaceholders(command string, analysisEntry inst.ReplicationAnalysis, successorInstance *inst.Instance) string {
	command = strings.Replace(command, "{failureType}", string(analysisEntry.Analysis), -1)
	command = strings.Replace(command, "{failureDescription}", analysisEntry.Description, -1)
	command = strings.Replace(command, "{failedHost}", analysisEntry.AnalyzedInstanceKey.Hostname, -1)
	command = strings.Replace(command, "{failedPort}", fmt.Sprintf("%d", analysisEntry.AnalyzedInstanceKey.Port), -1)
	command = strings.Replace(command, "{failureCluster}", analysisEntry.ClusterName, -1)
	command = strings.Replace(command, "{failureClusterAlias}", analysisEntry.ClusterAlias, -1)
	command = strings.Replace(command, "{countSlaves}", fmt.Sprintf("%d", analysisEntry.CountSlaves), -1)

	if successorInstance != nil {
		command = strings.Replace(command, "{successorHost}", successorInstance.Key.Hostname, -1)
		command = strings.Replace(command, "{successorPort}", fmt.Sprintf("%d", successorInstance.Key.Port), -1)
	}

	command = strings.Replace(command, "{slaveHosts}", analysisEntry.GetSlaveHostsAsString(), -1)

	return command
}

// filtersMatchAnalysisEntry will see whether the given filters apply for the given analysis entry (and hence the cluster it relates to)
func filtersMatchAnalysisEntry(analysisEntry inst.ReplicationAnalysis, filters []string, skipFilters bool) bool {
	if skipFilters {
		return true
	}
	for _, filter := range filters {
		if strings.HasPrefix(filter, "alias=") {
			// Match by exact cluster alias name
			alias := strings.SplitN(filter, "=", 2)[1]
			if alias == analysisEntry.ClusterAlias {
				return true
			}
		} else if strings.HasPrefix(filter, "alias~=") {
			// Match by cluster alias regex
			aliasPattern := strings.SplitN(filter, "~=", 2)[1]
			if matched, _ := regexp.MatchString(aliasPattern, analysisEntry.ClusterAlias); matched {
				return true
			}
		} else if matched, _ := regexp.MatchString(filter, analysisEntry.ClusterName); matched && filter != "" {
			return true
		}
	}
	return false
}

// executeProcesses executes a list of processes
func executeProcesses(processes []string, description string, analysisEntry inst.ReplicationAnalysis, successorInstance *inst.Instance, failOnError bool) error {
	var err error
	for _, command := range processes {
		command := replaceCommandPlaceholders(command, analysisEntry, successorInstance)

		if cmdErr := os.CommandRun(command); cmdErr == nil {
			log.Infof("Executed %s command: %s", description, command)
		} else {
			if err == nil {
				// Note first error
				err = cmdErr
			}
			log.Errorf("Failed to execute %s command: %s", description, command)
			if failOnError {
				return err
			}
		}
	}
	return err
}

func RecoverDeadMaster(analysisEntry inst.ReplicationAnalysis) (bool, *inst.Instance, error) {
	failedInstanceKey := &analysisEntry.AnalyzedInstanceKey
	if ok, err := AttemptRecoveryRegistration(&analysisEntry); !ok {
		log.Debugf("Will not RecoverDeadMaster on %+v", *failedInstanceKey)
		return false, nil, err
	}

	inst.AuditOperation("recover-dead-master", failedInstanceKey, "problem found; will recover")
	if err := executeProcesses(config.Config.PreFailoverProcesses, "PreFailoverProcesses", analysisEntry, nil, true); err != nil {
		return false, nil, err
	}

	log.Debugf("RecoverDeadMaster: will recover %+v", *failedInstanceKey)
	_, _, _, candidateSlave, err := inst.RegroupSlaves(failedInstanceKey, nil)

	ResolveRecovery(failedInstanceKey, &candidateSlave.Key)

	log.Debugf("- RecoverDeadMaster: candidate slave is %+v", candidateSlave.Key)
	inst.AuditOperation("recover-dead-master", failedInstanceKey, fmt.Sprintf("master: %+v", candidateSlave.Key))

	return true, candidateSlave, err
}

func replacePromotedSlaveWithCandidate(deadInstanceKey *inst.InstanceKey, promotedSlave *inst.Instance, candidateInstanceKey *inst.InstanceKey) (*inst.Instance, error) {
	var candidateSlaves [](*inst.Instance)
	if candidateInstanceKey == nil {
		// See if we can auto-figure out a candidate
		candidateSlaves, _ = inst.ReadClusterCandidateInstances(promotedSlave.ClusterName)
		for _, candidateSlave := range candidateSlaves {
			if promotedSlave.Key.Equals(&candidateSlave.Key) {
				// Seems like we promoted a candidate! We're happy!
				return promotedSlave, nil
			}
		}
	}
	// We didn't pick a candidate; let's offer one
	if candidateInstanceKey == nil {
		// Try a candidate slave that is in same DC & env as the dead instance
		if deadInstance, _, err := inst.ReadInstance(deadInstanceKey); err == nil && deadInstance != nil {
			for _, candidateSlave := range candidateSlaves {
				if deadInstance.DataCenter == deadInstance.DataCenter &&
					deadInstance.PhysicalEnvironment == deadInstance.PhysicalEnvironment &&
					candidateSlave.MasterKey.Equals(&promotedSlave.Key) {
					// This would make a good candidate
					candidateInstanceKey = &candidateSlave.Key
					log.Debugf("No candidate was offered for %+v but orchestrator picks %+v as candidate replacement, based on being in same DC & env as failed instance", promotedSlave.Key, candidateSlave.Key)
				}
			}
		}
	}
	// Still nothing?
	if candidateInstanceKey == nil {
		// Try a candidate slave that is in same DC & env as the promoted slave
		for _, candidateSlave := range candidateSlaves {
			if promotedSlave.DataCenter == candidateSlave.DataCenter &&
				promotedSlave.PhysicalEnvironment == candidateSlave.PhysicalEnvironment &&
				candidateSlave.MasterKey.Equals(&promotedSlave.Key) {
				// This would make a good candidate
				candidateInstanceKey = &candidateSlave.Key
				log.Debugf("No candidate was offered for %+v but orchestrator picks %+v as candidate replacement, based on being in same DC & env as promoted instance", promotedSlave.Key, candidateSlave.Key)
			}
		}
	}

	// So do we have a candidate?
	if candidateInstanceKey == nil {
		return promotedSlave, nil
	}
	if promotedSlave.Key.Equals(candidateInstanceKey) {
		// It IS the candidate
		return promotedSlave, nil
	}

	// Try and promote suggested candidate, if applicable and possible
	log.Debugf("Promoted instance %+v is not the suggested candidate %+v. Will see what can be done", promotedSlave.Key, *candidateInstanceKey)

	candidateInstance, _, err := inst.ReadInstance(candidateInstanceKey)
	if err != nil {
		return promotedSlave, log.Errore(err)
	}

	if candidateInstance.MasterKey.Equals(&promotedSlave.Key) {
		log.Debugf("Suggested candidate %+v is slave of promoted instance %+v. Will try and enslave its master", *candidateInstanceKey, promotedSlave.Key)
		candidateInstance, err = inst.EnslaveMaster(&candidateInstance.Key)
		if err != nil {
			return promotedSlave, log.Errore(err)
		}
		return candidateInstance, nil
	}

	log.Debugf("Could not manage to promoted suggested candidate %+v", *candidateInstanceKey)
	return promotedSlave, nil
}

// checkAndRecoverDeadMaster checks a given analysis, decides whether to take action, and possibly takes action
// Returns true when action was taken.
func checkAndRecoverDeadMaster(analysisEntry inst.ReplicationAnalysis, candidateInstanceKey *inst.InstanceKey, skipFilters bool) (bool, *inst.Instance, error) {
	if !filtersMatchAnalysisEntry(analysisEntry, config.Config.RecoverMasterClusterFilters, skipFilters) {
		return false, nil, nil
	}
	// Let's do dead master recovery!
	log.Debugf("Will handle DeadMaster event on %+v", analysisEntry.ClusterName)
	actionTaken, promotedSlave, err := RecoverDeadMaster(analysisEntry)

	if actionTaken && promotedSlave != nil {
		promotedSlave, _ = replacePromotedSlaveWithCandidate(&analysisEntry.AnalyzedInstanceKey, promotedSlave, candidateInstanceKey)
		// Execute post master-failover processes
		executeProcesses(config.Config.PostMasterFailoverProcesses, "PostMasterFailoverProcesses", analysisEntry, promotedSlave, false)
	}

	return actionTaken, promotedSlave, err
}

func isGeneralyValidAsCandidateSiblingOfIntermediateMaster(sibling *inst.Instance) bool {
	if !sibling.LogBinEnabled {
		return false
	}
	if !sibling.LogSlaveUpdatesEnabled {
		return false
	}
	if !sibling.SlaveRunning() {
		return false
	}
	if !sibling.IsLastCheckValid {
		return false
	}
	return true
}

func isValidAsCandidateSiblingOfIntermediateMaster(intermediateMasterInstance *inst.Instance, sibling *inst.Instance) bool {
	if sibling.Key.Equals(&intermediateMasterInstance.Key) {
		// same instance
		return false
	}
	if !isGeneralyValidAsCandidateSiblingOfIntermediateMaster(sibling) {
		return false
	}
	if sibling.DataCenter != intermediateMasterInstance.DataCenter {
		return false
	}
	if sibling.PhysicalEnvironment != intermediateMasterInstance.PhysicalEnvironment {
		return false
	}
	if sibling.HasReplicationFilters != intermediateMasterInstance.HasReplicationFilters {
		return false
	}
	if sibling.IsMaxScale() || intermediateMasterInstance.IsMaxScale() {
		// With MaxScale the failover is different; we don't need this "move to uncle" logic.
		return false
	}
	if sibling.ExecBinlogCoordinates.SmallerThan(&intermediateMasterInstance.ExecBinlogCoordinates) {
		return false
	}
	return true
}

// GetCandidateSlave chooses the best slave to promote given a (possibly dead) master
func GetCandidateSiblingOfIntermediateMaster(intermediateMasterKey *inst.InstanceKey) (*inst.Instance, error) {
	intermediateMasterInstance, _, err := inst.ReadInstance(intermediateMasterKey)
	if err != nil {
		return nil, err
	}

	siblings, err := inst.ReadSlaveInstances(&intermediateMasterInstance.MasterKey)
	if err != nil {
		return nil, err
	}
	if len(siblings) <= 1 {
		return nil, log.Errorf("No siblings found for %+v", *intermediateMasterKey)
	}

	sort.Sort(sort.Reverse(InstancesByCountSlaves(siblings)))

	for _, sibling := range siblings {
		sibling := sibling
		if isValidAsCandidateSiblingOfIntermediateMaster(intermediateMasterInstance, sibling) {
			// this is *assumed* to be a good choice.
			// We don't know for sure:
			// - the dead intermediate master's position may have been more advanced then last recorded
			// - and the candidate's position may have been stalled in the past seconds
			// But it's an attempt...
			return sibling, nil
		}
	}
	return nil, log.Errorf("Cannot find candidate sibling of %+v", *intermediateMasterKey)
}

func RecoverDeadIntermediateMaster(analysisEntry inst.ReplicationAnalysis) (actionTaken bool, successorInstance *inst.Instance, err error) {
	failedInstanceKey := &analysisEntry.AnalyzedInstanceKey
	if ok, err := AttemptRecoveryRegistration(&analysisEntry); !ok {
		log.Debugf("Will not RecoverDeadIntermediateMaster on %+v", *failedInstanceKey)
		return false, nil, err
	}

	inst.AuditOperation("recover-dead-intermediate-master", failedInstanceKey, "problem found; will recover")
	log.Debugf("RecoverDeadIntermediateMaster: will recover %+v", *failedInstanceKey)
	if err := executeProcesses(config.Config.PreFailoverProcesses, "PreFailoverProcesses", analysisEntry, nil, true); err != nil {
		return false, nil, err
	}

	if candidateSibling, err := GetCandidateSiblingOfIntermediateMaster(failedInstanceKey); err == nil {
		log.Debugf("- RecoverDeadIntermediateMaster: will attempt a candidate intermediate master: %+v", candidateSibling.Key)
		// We have a candidate
		if matchedSlaves, candidateSibling, err, errs := inst.MultiMatchSlaves(failedInstanceKey, &candidateSibling.Key, ""); err == nil {
			ResolveRecovery(failedInstanceKey, &candidateSibling.Key)

			successorInstance = candidateSibling
			actionTaken = true

			log.Debugf("- RecoverDeadIntermediateMaster: move to candidate intermediate master (%+v) went with %d errors", candidateSibling.Key, len(errs))
			inst.AuditOperation("recover-dead-intermediate-master", failedInstanceKey, fmt.Sprintf("Done. Matched %d slaves under candidate sibling: %+v; %d errors: %+v", len(matchedSlaves), candidateSibling.Key, len(errs), errs))
		} else {
			log.Debugf("- RecoverDeadIntermediateMaster: move to candidate intermediate master (%+v) did not complete: %+v", candidateSibling.Key, err)
			inst.AuditOperation("recover-dead-intermediate-master", failedInstanceKey, fmt.Sprintf("Matched %d slaves under candidate sibling: %+v; %d errors: %+v", len(matchedSlaves), candidateSibling.Key, len(errs), errs))
		}
	}
	if !actionTaken {
		// Either no candidate or only partial match of slaves. Regroup as plan B
		inst.RegroupSlaves(failedInstanceKey, nil)
		// We don't care much if regroup made it or not. We prefer that it made it, in whcih case we only need to match up
		// one slave, but the operation is still valid if regroup partially/completely failed. We just promote anything
		// not regrouped.
		// So, match up all that's left, plan C
		log.Debugf("- RecoverDeadIntermediateMaster: will next attempt a match up from %+v", *failedInstanceKey)

		var errs []error
		var matchedSlaves [](*inst.Instance)
		matchedSlaves, successorInstance, err, errs = inst.MatchUpSlaves(failedInstanceKey, "")
		if len(matchedSlaves) == 0 {
			log.Errorf("RecoverDeadIntermediateMaster failed to match up any slave from %+v", *failedInstanceKey)
			return false, successorInstance, err
		}
		ResolveRecovery(failedInstanceKey, &successorInstance.Key)
		actionTaken = true

		log.Debugf("- RecoverDeadIntermediateMaster: matched up to %+v", successorInstance.Key)
		inst.AuditOperation("recover-dead-intermediate-master", failedInstanceKey, fmt.Sprintf("Done. Matched slaves under: %+v %d errors: %+v", successorInstance.Key, len(errs), errs))
	}
	return actionTaken, successorInstance, err
}

// checkAndRecoverDeadIntermediateMaster checks a given analysis, decides whether to take action, and possibly takes action
// Returns true when action was taken.
func checkAndRecoverDeadIntermediateMaster(analysisEntry inst.ReplicationAnalysis, candidateInstanceKey *inst.InstanceKey, skipFilters bool) (bool, *inst.Instance, error) {
	if !filtersMatchAnalysisEntry(analysisEntry, config.Config.RecoverIntermediateMasterClusterFilters, skipFilters) {
		return false, nil, nil
	}

	actionTaken, promotedSlave, err := RecoverDeadIntermediateMaster(analysisEntry)
	if actionTaken {
		// Execute post intermediate-master-failover processes
		executeProcesses(config.Config.PostIntermediateMasterFailoverProcesses, "PostIntermediateMasterFailoverProcesses", analysisEntry, promotedSlave, false)
	}
	return actionTaken, promotedSlave, err
}

// Force a re-read of a topology instance; this is done because we need to substantiate a suspicion that we may have a failover
// scenario. we want to speed up rading the complete picture.
func emergentlyReadTopologyInstance(instanceKey *inst.InstanceKey, analysisCode inst.AnalysisCode) {
	if err := emergencyReadTopologyInstanceMap.Add(instanceKey.DisplayString(), true, 0); err == nil {
		emergencyReadTopologyInstanceMap.Set(instanceKey.DisplayString(), true, 0)
		go inst.ExecuteOnTopology(func() {
			inst.ReadTopologyInstance(instanceKey)
			inst.AuditOperation("emergently-read-topology-instance", instanceKey, string(analysisCode))
		})
	}
}

// Force reading of slaves of given instance. This is because we suspect the instance is dead, and want to speed up
// detection of replication failure from its slaves.
func emergentlyReadTopologyInstanceSlaves(instanceKey *inst.InstanceKey, analysisCode inst.AnalysisCode) {
	slaves, err := inst.ReadSlaveInstances(instanceKey)
	if err != nil {
		return
	}
	for _, slave := range slaves {
		go emergentlyReadTopologyInstance(&slave.Key, analysisCode)
	}
}

// executeCheckAndRecoverFunction will choose the correct check & recovery function based on analysis.
// It executes the function synchronuously
func executeCheckAndRecoverFunction(analysisEntry inst.ReplicationAnalysis, candidateInstanceKey *inst.InstanceKey, skipFilters bool) (bool, *inst.Instance, error) {
	var checkAndRecoverFunction func(analysisEntry inst.ReplicationAnalysis, candidateInstanceKey *inst.InstanceKey, skipFilters bool) (bool, *inst.Instance, error) = nil

	switch analysisEntry.Analysis {
	case inst.DeadMaster:
		checkAndRecoverFunction = checkAndRecoverDeadMaster
	case inst.DeadMasterAndSomeSlaves:
		checkAndRecoverFunction = checkAndRecoverDeadMaster
	case inst.DeadIntermediateMaster:
		checkAndRecoverFunction = checkAndRecoverDeadIntermediateMaster
	case inst.DeadIntermediateMasterAndSomeSlaves:
		checkAndRecoverFunction = checkAndRecoverDeadIntermediateMaster
	case inst.DeadCoMaster:
		checkAndRecoverFunction = checkAndRecoverDeadIntermediateMaster
	case inst.UnreachableMaster:
		go emergentlyReadTopologyInstanceSlaves(&analysisEntry.AnalyzedInstanceKey, analysisEntry.Analysis)
	case inst.AllMasterSlavesNotReplicating:
		go emergentlyReadTopologyInstance(&analysisEntry.AnalyzedInstanceKey, analysisEntry.Analysis)
	case inst.FirstTierSlaveFailingToConnectToMaster:
		go emergentlyReadTopologyInstance(&analysisEntry.AnalyzedInstanceMasterKey, analysisEntry.Analysis)
	}

	if checkAndRecoverFunction == nil {
		// Unhandled problem type
		return false, nil, nil
	}
	// we have a recovery function; its execution still depends on filters if not disabled.

	// Execute on-detection processes
	if err := executeProcesses(config.Config.OnFailureDetectionProcesses, "OnFailureDetectionProcesses", analysisEntry, nil, true); err != nil {
		return false, nil, err
	}

	actionTaken, promotedSlave, err := checkAndRecoverFunction(analysisEntry, candidateInstanceKey, skipFilters)
	if actionTaken {
		// Execute post intermediate-master-failover processes
		executeProcesses(config.Config.PostFailoverProcesses, "PostFailoverProcesses", analysisEntry, promotedSlave, false)
	}
	return actionTaken, promotedSlave, err
}

// CheckAndRecover is the main entry point for the recovery mechanism
func CheckAndRecover(specificInstance *inst.InstanceKey, candidateInstanceKey *inst.InstanceKey, skipFilters bool) (actionTaken bool, instance *inst.Instance, err error) {
	replicationAnalysis, err := inst.GetReplicationAnalysis(true)
	if err != nil {
		return false, nil, log.Errore(err)
	}
	for _, analysisEntry := range replicationAnalysis {
		if specificInstance != nil {
			// We are looking for a specific instance; if this is not the one, skip!
			if !specificInstance.Equals(&analysisEntry.AnalyzedInstanceKey) {
				continue
			}
		}
		if analysisEntry.IsDowntimed && specificInstance == nil {
			// Only recover a downtimed server if explicitly requested
			continue
		}

		if specificInstance != nil && skipFilters {
			// force mode. Keep it synchronuous
			actionTaken, instance, err = executeCheckAndRecoverFunction(analysisEntry, candidateInstanceKey, skipFilters)
		} else {
			go executeCheckAndRecoverFunction(analysisEntry, candidateInstanceKey, skipFilters)
		}
	}
	return actionTaken, instance, err
}
