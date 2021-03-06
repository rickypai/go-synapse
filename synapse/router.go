package synapse

import (
	"encoding/json"
	"github.com/n0rad/go-erlog/data"
	"github.com/n0rad/go-erlog/errs"
	"github.com/n0rad/go-erlog/logs"
	"sync"
	"time"
)

type RouterCommon struct {
	Type                        string
	EventsBufferDurationInMilli int
	Services                    []*Service

	synapse    *Synapse
	lastEvents map[*Service]*ServiceReport
	fields     data.Fields
}

type Router interface {
	Init(s *Synapse) error
	getFields() data.Fields
	Run(context *ContextImpl)
	Update(serviceReports []ServiceReport) error
	ParseServerOptions(data []byte) (interface{}, error)
	ParseRouterOptions(data []byte) (interface{}, error)
}

func (r *RouterCommon) commonInit(router Router, synapse *Synapse) error {
	r.fields = data.WithField("type", r.Type)
	r.synapse = synapse

	if r.EventsBufferDurationInMilli == 0 {
		r.EventsBufferDurationInMilli = 500
	}

	r.lastEvents = make(map[*Service]*ServiceReport)
	for _, service := range r.Services {
		if err := service.Init(router, synapse); err != nil {
			return errs.WithEF(err, r.fields, "Failed to init service")
		}
	}

	return nil
}

func (r *RouterCommon) RunCommon(context *ContextImpl, router Router) {
	context.doneWaiter.Add(1)
	defer context.doneWaiter.Done()

	events := make(chan ServiceReport)
	watcherContext := newContext(context.oneshot)
	for _, service := range r.Services {
		go service.typedWatcher.Watch(watcherContext, events, service)
	}

	go r.eventsProcessor(events, router)

	<-context.stop
	close(watcherContext.stop)
	watcherContext.doneWaiter.Wait()
	logs.WithF(r.fields).Debug("All Watchers stopped")
	close(events)
}

func (r *RouterCommon) eventsProcessor(events chan ServiceReport, router Router) {
	updateMutex := sync.Mutex{}
	bufEvents := make(map[*Service]*ServiceReport)
	var eventsTimer *time.Timer

	deferRun := func() {
		logs.WithF(r.fields.WithField("events", bufEvents)).Debug("Run events buffer")
		updateMutex.Lock()
		reports := []ServiceReport{}
		for _, s := range bufEvents {
			reports = append(reports, *s)
		}
		bufEvents = make(map[*Service]*ServiceReport)
		updateMutex.Unlock()

		r.handleReport(reports, router)
	}

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}

			logs.WithF(r.fields.WithField("event", event)).Debug("Router received an event")
			if eventsTimer != nil && !eventsTimer.Stop() {
				logs.WithF(r.fields.WithField("event", event)).Trace("Event Already fired")
			} else {
				logs.WithF(r.fields.WithField("event", event)).Trace("Event Added to buffer")
			}

			updateMutex.Lock()
			bufEvents[event.Service] = &event
			updateMutex.Unlock()
			eventsTimer = time.AfterFunc(time.Duration(r.EventsBufferDurationInMilli)*time.Millisecond, deferRun)
		}
	}
}

func (r *RouterCommon) handleReport(events []ServiceReport, router Router) {
	validEvents := []ServiceReport{}

	for _, event := range events {

		event.Service.ServerSort.Sort(&event.Reports)

		available, unavailable := event.AvailableUnavailable()
		r.synapse.serviceAvailableCount.WithLabelValues(event.Service.Name).Set(float64(available))
		r.synapse.serviceUnavailableCount.WithLabelValues(event.Service.Name).Set(float64(unavailable))

		if !event.HasActiveServers() {
			if r.lastEvents[event.Service] == nil {
				logs.WithF(event.Service.fields).Warn("First Report has no active server. Not declaring in router")
			} else {
				logs.WithF(event.Service.fields).Error("Receiving report with no active server. Keeping previous report")
			}
			continue
		} else if r.lastEvents[event.Service] == nil || r.lastEvents[event.Service].HasActiveServers() != event.HasActiveServers() {
			logs.WithF(event.Service.fields.WithField("event", event)).Info("Server(s) available for router")
		}
		validEvents = append(validEvents, event)
	}

	if len(validEvents) == 0 {
		logs.WithF(r.fields).Debug("Nothing to update on router")
		return
	}

	if err := router.Update(validEvents); err != nil {
		r.synapse.routerUpdateFailures.WithLabelValues(r.Type).Inc()
		logs.WithEF(err, r.fields).Error("Failed to report watch modification")
	}

	for _, e := range validEvents {
		r.lastEvents[e.Service] = &e
	}
}

func (r *RouterCommon) getFields() data.Fields {
	return r.fields
}

func RouterFromJson(content []byte, s *Synapse) (Router, error) {
	t := &RouterCommon{}
	if err := json.Unmarshal([]byte(content), t); err != nil {
		return nil, errs.WithE(err, "Failed to unmarshall check type")
	}

	fields := data.WithField("type", t.Type)
	var typedRouter Router
	switch t.Type {
	case "console":
		typedRouter = NewRouterConsole()
	case "haproxy":
		typedRouter = NewRouterHaProxy()
	case "template":
		typedRouter = NewRouterTemplate()
	default:
		return nil, errs.WithF(fields, "Unsupported router type")
	}

	if err := json.Unmarshal([]byte(content), &typedRouter); err != nil {
		return nil, errs.WithEF(err, fields, "Failed to unmarshall router")
	}

	if err := typedRouter.Init(s); err != nil {
		return nil, errs.WithEF(err, fields, "Failed to init router")
	}
	return typedRouter, nil
}
