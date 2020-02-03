package changes

import (
	"fmt"
	"github.com/boltdb/bolt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/eventlog"
	"github.com/open-horizon/anax/events"
	"github.com/open-horizon/anax/exchange"
	"github.com/open-horizon/anax/i18n"
	"github.com/open-horizon/anax/persistence"
	"github.com/open-horizon/anax/policy"
	"github.com/open-horizon/anax/version"
	"github.com/open-horizon/anax/worker"
	"strings"
	"time"
)

type ChangesWorker struct {
	worker.BaseWorker // embedded field
	db                *bolt.DB
	pollInterval      int    // The current change polling interval. This interval will float between Min and Max intervals.
	pollMinInterval   int    // The minimum time to wait between polls to the exchange.
	pollMaxInterval   int    // The maximum time to wait between polls to the exchange.
	pollAdjustment    int    // The amount to increase the polling time, each time it is increased.
	noMsgCount        int    // How many consecutive polls have returned no changes.
	agreementReached  bool   // True when ths node has seen at least one agreement.
	changeID          uint64 // The current change Id in the exchange.
	lastHeartbeat     int64  // Last time a heartbeat was successful.
	heartBeatFailed   bool   // Remember that the heartbeat has failed.
	noworkDispatch    int64  // The last time the NoWorkHandler was dispatched.
}

func NewChangesWorker(name string, cfg *config.HorizonConfig, db *bolt.DB) *ChangesWorker {

	var ec *worker.BaseExchangeContext
	dev, _ := persistence.FindExchangeDevice(db)
	if dev != nil {
		ec = worker.NewExchangeContext(fmt.Sprintf("%v/%v", dev.Org, dev.Id), dev.Token, cfg.Edge.ExchangeURL, cfg.GetCSSURL(), newLimitedRetryHTTPFactory(cfg.Collaborators.HTTPClientFactory))
	}

	worker := &ChangesWorker{
		BaseWorker:       worker.NewBaseWorker(name, cfg, ec),
		db:               db,
		pollInterval:     config.ExchangeMessagePollInterval_DEFAULT,
		pollMinInterval:  config.ExchangeMessagePollInterval_DEFAULT,
		pollMaxInterval:  config.ExchangeMessagePollMaxInterval_DEFAULT,
		pollAdjustment:   config.ExchangeMessagePollIncrement_DEFAULT,
		noMsgCount:       0,
		agreementReached: false,
		changeID:         0,
		heartBeatFailed:  false,
		noworkDispatch:   time.Now().Unix(),
	}

	// Initialize the change state tracking from the local DB.
	chgState, err := persistence.FindExchangeChangeState(db)
	if err != nil {
		glog.Errorf(chglog(fmt.Sprintf("error searching for persistent exchange change state, error %v", err)))
	}

	if chgState != nil && chgState.ChangeID != 0 {
		worker.changeID = chgState.ChangeID
		glog.V(3).Info(chglog(fmt.Sprintf("restore exchange change state after restart: %v", chgState)))
	}

	glog.Info(chglog(fmt.Sprintf("Starting ExchangeChanges worker")))

	// The initial poll interval is changed dynamically by the NoWorkHandler when it detects that it can increase
	// or decrease the polling interval.
	worker.Start(worker, config.ExchangeMessagePollInterval_DEFAULT)
	return worker
}

// Customized HTTPFactory for limiting retries.
func newLimitedRetryHTTPFactory(base *config.HTTPClientFactory) *config.HTTPClientFactory {
	limitedRetryHTTPFactory := &config.HTTPClientFactory{
		NewHTTPClient: base.NewHTTPClient,
		RetryCount:    2,
		RetryInterval: 3,
	}
	return limitedRetryHTTPFactory
}

func (w *ChangesWorker) Messages() chan events.Message {
	return w.BaseWorker.Manager.Messages
}

func (w *ChangesWorker) Initialize() bool {

	// If there are already agreements, then we can allow the polling interval to grow. If not, the first agreement
	// that gets made will allow the poller interval to grow.
	if agreements, err := persistence.FindEstablishedAgreementsAllProtocols(w.db, policy.AllAgreementProtocols(), []persistence.EAFilter{persistence.UnarchivedEAFilter()}); err != nil {
		glog.Errorf(chglog(fmt.Sprintf("error searching for agreements, error %v", err)))
	} else if len(agreements) != 0 {
		w.agreementReached = true
	}

	// If we havent picked up changes yet, make sure to broadcast all change events just to make sure the device is
	// up to date with what's in the exchange. If there is previous change state in the local DB, then the change ID
	// will be non-zero. This logic is placed here (and not the constructor) because we dont want to emit messages
	// to the message bus from inside the worker constructor.
	if w.changeID == 0 && w.GetExchangeToken() != "" {

		// Call the exchange to retrieve the current max change id. Use a custom exchange context that blocks (retries forever)
		// until we can get the max change ID.
		ec := exchange.NewCustomExchangeContext(w.EC.Id, w.EC.Token, w.EC.URL, w.EC.CSSURL, w.Config.Collaborators.HTTPClientFactory)
		if maxChangeID, err := exchange.GetHTTPExchangeMaxChangeIDHandler(ec)(); err != nil {
			glog.Errorf(chglog(fmt.Sprintf("Error retrieving max change ID, error: %v", err)))
		} else {
			w.changeID = maxChangeID.MaxChangeID
			if err := persistence.SaveExchangeChangeState(w.db, w.changeID); err != nil {
				glog.Errorf(chglog(fmt.Sprintf("error saving persistent exchange change state, error %v", err)))
			}
		}
		supportedResourceTypes := w.createSupportedResourceTypes(true)
		w.emitChangeMessages(supportedResourceTypes)
	}

	// Retrieve the node's heartbeat intervals if we can.
	if w.GetExchangeToken() != "" {
		w.getNodeHeartbeatIntervals()
	}

	return true
}

// Handle events that are propogated to this worker from the internal event bus.
func (w *ChangesWorker) NewEvent(incoming events.Message) {

	switch incoming.(type) {

	case *events.EdgeRegisteredExchangeMessage:
		msg, _ := incoming.(*events.EdgeRegisteredExchangeMessage)
		w.Commands <- NewDeviceRegisteredCommand(msg)

	case *events.AgreementReachedMessage:
		w.Commands <- NewAgreementCommand()

	case *events.NodePolicyMessage:
		w.Commands <- NewResetIntervalCommand()

	case *events.NodeUserInputMessage:
		w.Commands <- NewResetIntervalCommand()

	case *events.GovernanceWorkloadCancelationMessage:
		msg, _ := incoming.(*events.GovernanceWorkloadCancelationMessage)
		switch msg.Event().Id {
		case events.AGREEMENT_ENDED:
			w.Commands <- NewResetIntervalCommand()
		}

	case *events.ApiAgreementCancelationMessage:
		msg, _ := incoming.(*events.ApiAgreementCancelationMessage)
		switch msg.Event().Id {
		case events.AGREEMENT_ENDED:
			w.Commands <- NewResetIntervalCommand()
		}

	case *events.ExchangeChangesShutdownMessage:
		msg, _ := incoming.(*events.ExchangeChangesShutdownMessage)
		switch msg.Event().Id {
		case events.MESSAGE_STOP:
			w.Commands <- worker.NewTerminateCommand("shutdown")
		}

	case *events.NodeShutdownCompleteMessage:
		msg, _ := incoming.(*events.NodeShutdownCompleteMessage)
		switch msg.Event().Id {
		case events.UNCONFIGURE_COMPLETE:
			w.Commands <- worker.NewTerminateCommand("shutdown")
		}

	default: //nothing

	}

	return
}

// Handle commands that are placed on the command queue.
func (w *ChangesWorker) CommandHandler(command worker.Command) bool {

	switch command.(type) {
	case *ResetIntervalCommand:
		w.resetPollingInterval()

		// When the command handler gets called by the worker framework, the noworkhandler timer is restarted.
		// Therefore, if there is a steady flow of commands coming into the command handler, the noworkhandler
		// might never get control. Given that, we will sometimes check for changes in the exchange outside the
		// noworkhandler if it hasnt been dispatched in a while.
		if w.GetExchangeToken() != "" && (time.Since(time.Unix(w.noworkDispatch, 0)).Seconds() >= float64(w.pollInterval)) {
			glog.V(5).Infof(chglog(fmt.Sprintf("early dispatch checking for changes")))
			w.findAndProcessChanges()
		}

	case *AgreementCommand:
		w.agreementReached = true

	case *DeviceRegisteredCommand:
		cmd, _ := command.(*DeviceRegisteredCommand)
		w.handleDeviceRegistration(cmd)

	default:
		return false
	}
	return true

}

// This function gets called when the worker framework has found nothing to do for the "no work interval"
// that was set when the worker was started. The "no work interval" can be changed while running in this
// function so that a worker can alter how often it wakes up to do maintenance.
func (w *ChangesWorker) NoWorkHandler() {

	// Dont poll for changes until the device is registered.
	if w.GetExchangeToken() == "" {
		glog.V(3).Infof(chglog(fmt.Sprintf("waiting for exchange registration")))
		return
	}

	// Heartbeat and check for changes.
	w.findAndProcessChanges()

	return
}

// Go get the latest changes and process them, notifying other workers that they might have work to do.
func (w *ChangesWorker) findAndProcessChanges() {

	w.noworkDispatch = time.Now().Unix()

	// If there is no last known change id, then we havent initialized yet,so do nothing.
	maxRecords := 1000
	if w.changeID == 0 {
		glog.Warningf(chglog(fmt.Sprintf("No starting change ID")))
		return
	}

	glog.V(3).Infof(chglog(fmt.Sprintf("looking for changes starting from ID %v", w.changeID)))

	// Call the exchange to retrieve any changes since our last known change id.
	changes, err := exchange.GetHTTPExchangeChangeHandler(w)(w.changeID, maxRecords)

	// Handle heartbeat state changes and errors. Returns true if there was an error to be handled.
	if w.handleHeartbeatStateAndError(changes, err) {
		return
	}

	// Loop through each change to identify resources that we are interested in, and then send out event messages
	// to notify the other workers that they have some work to do.
	resourceTypes := w.createSupportedResourceTypes(false)
	for _, change := range changes.Changes {
		glog.V(3).Infof(chglog(fmt.Sprintf("Change: %v", change)))

		if change.IsMessage(w.GetExchangeId()) {
			resourceTypes[events.CHANGE_MESSAGE_TYPE] = true
		} else if change.IsNode(w.GetExchangeId()) {
			resourceTypes[events.CHANGE_NODE_TYPE] = true
			w.getNodeHeartbeatIntervals()
		} else if change.IsNodePolicy(w.GetExchangeId()) {
			resourceTypes[events.CHANGE_NODE_POLICY_TYPE] = true
		} else if change.IsNodeError(w.GetExchangeId()) {
			resourceTypes[events.CHANGE_NODE_ERROR_TYPE] = true
		} else if change.IsService() {
			resourceTypes[events.CHANGE_SERVICE_TYPE] = true
		} else {
			glog.V(5).Infof(chglog(fmt.Sprintf("Unhandled change: %v %v/%v", change.Resource, change.OrgID, change.ID)))
		}
	}

	emittedMessages := w.emitChangeMessages(resourceTypes)

	// Record the most recent change id and reset the polling interval based on the changes that were found.
	w.postProcessChanges(changes, emittedMessages)

	glog.V(3).Infof(chglog(fmt.Sprintf("done looking for changes")))

}

// Create a map of exchange resources that a device cares about. The resources in the map are set to boolean
// true if initialValue is set to true. This enables the map to be passed to emitChangeMessages so that a
// change message is sent to all workers.
func (w *ChangesWorker) createSupportedResourceTypes(initialValue bool) map[events.EventId]bool {
	resourceTypes := make(map[events.EventId]bool)
	resourceTypes[events.CHANGE_MESSAGE_TYPE] = initialValue
	resourceTypes[events.CHANGE_NODE_TYPE] = initialValue
	resourceTypes[events.CHANGE_NODE_POLICY_TYPE] = initialValue
	resourceTypes[events.CHANGE_NODE_ERROR_TYPE] = initialValue
	resourceTypes[events.CHANGE_SERVICE_TYPE] = initialValue
	return resourceTypes
}

// Send change message for each change type in the map that is set to true.
func (w *ChangesWorker) emitChangeMessages(resChanges map[events.EventId]bool) bool {
	emitMessage := false
	for changeType, _ := range resChanges {
		if resChanges[changeType] {
			emitMessage = true
			w.Messages() <- events.NewExchangeChangeMessage(changeType)
		}
	}
	return emitMessage
}

// Record the most recent change id and reset the polling interval based on the changes that were found.
func (w *ChangesWorker) postProcessChanges(changes *exchange.ExchangeChanges, interestingChanges bool) {

	// If there were changes found, even uninteresting changes, we need to keep the most recent change id current.
	if changes.GetMostRecentChangeID() != 0 {
		w.changeID = changes.GetMostRecentChangeID() + 1
		if err := persistence.SaveExchangeChangeState(w.db, w.changeID); err != nil {
			glog.Errorf(chglog(fmt.Sprintf("error saving persistent exchange change state, error %v", err)))
		}
	}

	// If we found interesting events, then make sure we keep the polling interval short. This way, a flood
	// of uninteresting changes will not cause us to incorrectly shorten the polling interval.

	// Recalculate a new polling interval if necessary.
	if w.Config.Edge.ExchangeMessageDynamicPoll {
		w.updatePollingInterval(interestingChanges)
	}
}

// Process any error from the /changes API and update the heartbeat state appropriately. Return true if the
// caller should not proceeed to process the response.
func (w *ChangesWorker) handleHeartbeatStateAndError(changes *exchange.ExchangeChanges, err error) bool {
	if err != nil {
		glog.Errorf(chglog(fmt.Sprintf("heartbeat and change retrieval failed, error %v", err)))

		if strings.Contains(err.Error(), "status: 401") {
			// If the heartbeat fails because the node entry is gone then initiate a full node quiesce.
			w.Messages() <- events.NewNodeShutdownMessage(events.START_UNCONFIGURE, false, false)
		} else {
			// The exchange context is configured for minimal retries and a small interval. This will cause retries
			// to end quickly and to be handled like errors here. When there are errors, the "no work interval" is kept
			// minimal so that the worker itself will retry very soon.
			w.resetPollingInterval()

			// If the heartbeat has been failing for the configured grace period, let other workers know that the heartbeat
			// has failed. The message is sent out only when the heartbeat state changes from success to failed after the configured
			// time limit for a heartbeat failure. The heartbeat could have failed because the exchange is under load and we are
			// unable to connect to it, the node might still have network connectivity so there is a grace period before declaring
			// that there is a heartbeat problem.
			if !w.heartBeatFailed && time.Since(time.Unix(w.lastHeartbeat, 0)).Seconds() > float64(w.Config.Edge.ExchangeHeartbeat) {
				w.heartBeatFailed = true

				eventlog.LogNodeEvent(w.db, persistence.SEVERITY_ERROR,
					persistence.NewMessageMeta(EL_AG_NODE_HB_FAILED, exchange.GetOrg(w.GetExchangeId()), exchange.GetId(w.GetExchangeId()), err.Error()),
					persistence.EC_NODE_HEARTBEAT_FAILED, exchange.GetId(w.GetExchangeId()), exchange.GetOrg(w.GetExchangeId()), "", "")

				w.Messages() <- events.NewNodeHeartbeatStateChangeMessage(events.NODE_HEARTBEAT_FAILED, exchange.GetOrg(w.GetExchangeId()), exchange.GetId(w.GetExchangeId()))
			}
		}
		return true
	} else {
		// Record the last good heartbeat
		w.lastHeartbeat = time.Now().Unix()

		// The node could be transitioning from disconnected to connected state.
		if w.heartBeatFailed {
			// Let other workers know that the heartbeat is restored. The message is sent out only when the heartbeat state
			// changes from failed to successful.
			w.heartBeatFailed = false

			glog.V(3).Infof(chglog(fmt.Sprintf("node heartbeat restored")))
			eventlog.LogNodeEvent(w.db, persistence.SEVERITY_INFO,
				persistence.NewMessageMeta(EL_AG_NODE_HB_RESTORED, exchange.GetOrg(w.GetExchangeId()), exchange.GetId(w.GetExchangeId())),
				persistence.EC_NODE_HEARTBEAT_RESTORED, exchange.GetId(w.GetExchangeId()), exchange.GetOrg(w.GetExchangeId()), "", "")

			w.Messages() <- events.NewNodeHeartbeatStateChangeMessage(events.NODE_HEARTBEAT_RESTORED, exchange.GetOrg(w.GetExchangeId()), exchange.GetId(w.GetExchangeId()))
		}

		// There is no error, but also no response object, that's a problem that needs to be logged.
		if changes == nil {
			glog.Errorf(chglog(fmt.Sprintf("Exchange /changes API returned no error and no response.")))
			return true
		}

		// Log an error if the current exchange version does not meet the minimum version requirement.
		if changes.GetExchangeVersion() != "" {
			if err := version.VerifyExchangeVersion1(changes.GetExchangeVersion(), false); err != nil {
				glog.Errorf(chglog(fmt.Sprintf("Error verifiying exchange version, error: %v", err)))
			}
		}

	}
	return false
}

// A stepping function for slowly increasing the time interval between polls to the exchange for changes.
// If there are no agreements, leave the polling interval alone.
// If a msg was received in the last interval, reduce the interval to the starting configured interval.
// Otherwise, increase the polling interval if enough polls have passed. The algorithm is a simple function that
// slowly increases the polling interval at first and then increases it's length more quickly as more and more
// polls come back with no messages.
func (w *ChangesWorker) updatePollingInterval(msgReceived bool) {

	if msgReceived {
		w.resetPollingInterval()
		return
	}

	if !w.agreementReached || (w.pollInterval >= w.pollMaxInterval) {
		return
	}

	w.noMsgCount += 1

	if w.noMsgCount >= (w.pollMaxInterval / w.pollInterval) {
		w.pollInterval += w.pollAdjustment
		if w.pollInterval > w.pollMaxInterval {
			w.pollInterval = w.pollMaxInterval
		}
		w.noMsgCount = 0
		w.SetNoWorkInterval(w.pollInterval)
		glog.V(3).Infof(chglog(fmt.Sprintf("Increasing change poll interval to %v, increment is %v", w.pollInterval, w.pollAdjustment)))
	}

	return

}

// Reset the polling interval step function to its starting values because there is activity in the system which might
// cause changes to occur in the exchange.
func (w *ChangesWorker) resetPollingInterval() {
	if w.pollInterval != w.pollMinInterval {
		w.pollInterval = w.pollMinInterval
		w.SetNoWorkInterval(w.pollInterval)
		glog.V(3).Infof(chglog(fmt.Sprintf("Resetting poll interval to %v, increment is %v", w.pollInterval, w.pollAdjustment)))
	}
	w.noMsgCount = 0
	return
}

// This function gets called when the device registers and is assigned an id and token which can be used to authenticate
// with the exchange.
func (w *ChangesWorker) handleDeviceRegistration(cmd *DeviceRegisteredCommand) {
	msg := cmd.Msg
	w.EC = worker.NewExchangeContext(fmt.Sprintf("%v/%v", msg.Org(), msg.DeviceId()), msg.Token(), w.Config.Edge.ExchangeURL, w.Config.GetCSSURL(), newLimitedRetryHTTPFactory(w.Config.Collaborators.HTTPClientFactory))

	// Retrieve the node's heartbeat configuration from the node itself, and update the worker.
	w.getNodeHeartbeatIntervals()

	// Call the exchange to retrieve the current max change id. Use a custom exchange context that blocks (retries forever)
	// until we can get the max change ID.
	ec := exchange.NewCustomExchangeContext(w.EC.Id, w.EC.Token, w.EC.URL, w.EC.CSSURL, w.Config.Collaborators.HTTPClientFactory)
	if maxChangeID, err := exchange.GetHTTPExchangeMaxChangeIDHandler(ec)(); err != nil {
		glog.Errorf(chglog(fmt.Sprintf("Error retrieving max change ID, error: %v", err)))
	} else {
		w.changeID = maxChangeID.MaxChangeID
		if err := persistence.SaveExchangeChangeState(w.db, w.changeID); err != nil {
			glog.Errorf(chglog(fmt.Sprintf("error saving persistent exchange change state, error %v", err)))
		}
	}

	// Safety measure to ensure that the agent has the latest info from the exchange.
	supportedResourceTypes := w.createSupportedResourceTypes(true)
	w.emitChangeMessages(supportedResourceTypes)

}

// This function gets called retrieve the node's heartbeat configuration, if there is any.
func (w *ChangesWorker) getNodeHeartbeatIntervals() {

	// Retrieve the node's heartbeat configuration from the node itself.
	if node, err := exchange.GetHTTPDeviceHandler(w)(w.GetExchangeId(), ""); err != nil {
		glog.Errorf(chglog(fmt.Sprintf("Error retrieving node %v heartbeat intervals, error: %v", w.GetExchangeId(), err)))
	} else {
		updated := false
		if node.HeartbeatIntv.MinInterval != 0 {
			w.pollMinInterval = node.HeartbeatIntv.MinInterval
			updated = true
		}
		if node.HeartbeatIntv.MaxInterval != 0 {
			w.pollMaxInterval = node.HeartbeatIntv.MaxInterval
			updated = true
		}
		if node.HeartbeatIntv.IntervalAdjustment != 0 {
			w.pollAdjustment = node.HeartbeatIntv.IntervalAdjustment
			updated = true
		}

		// Reconcile the new values with the existing poll interval, by resetting the poll interval to the starting value.
		if updated {
			glog.V(3).Infof(chglog(fmt.Sprintf("Heartbeat Poll intervals from node min: %v, max: %v, increment: %v", w.pollMinInterval, w.pollMaxInterval, w.pollAdjustment)))
			w.resetPollingInterval()
		}

	}
}

// Utility logging function
var chglog = func(v interface{}) string {
	return fmt.Sprintf("Exchange Changes Worker: %v", v)
}

// messages for eventlog
const (
	EL_AG_NODE_HB_FAILED   = "Node heartbeat failed for node %v/%v. Error: %v"
	EL_AG_NODE_HB_RESTORED = "Node heartbeat restored for node %v/%v."
)

// This is does nothing useful at run time.
// This code is only used at compile time to make the eventlog messages get into the catalog so that
// they can be translated.
// The event log messages will be saved in English. But the CLI can request them in different languages.
func MarkI18nMessages() {
	// get message printer. anax default language is English
	msgPrinter := i18n.GetMessagePrinter()

	msgPrinter.Sprintf(EL_AG_NODE_HB_FAILED)
	msgPrinter.Sprintf(EL_AG_NODE_HB_RESTORED)
}