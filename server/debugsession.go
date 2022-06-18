package server

import (
	"bufio"
	"context"
	"encoding/json"
	"sync"

	"github.com/google/go-dap"
	"github.com/onflow/cadence/runtime/interpreter"
	"github.com/sirupsen/logrus"

	"github.com/onflow/flow-emulator/server/backend"
)

type debugSession struct {
	logger     *logrus.Logger
	backend    *backend.Backend
	readWriter *bufio.ReadWriter

	// sendQueue is used to capture messages from multiple request
	// processing goroutines while writing them to the client connection
	// from a single goroutine via sendFromQueue.
	//
	// We must keep track of the multiple channel senders with a wait group
	// to make sure we do not close this channel prematurely.
	//
	// Closing this channel will signal the sendFromQueue goroutine that it can exit.
	sendQueue chan dap.Message
	sendWg    sync.WaitGroup
	// debugger is the current
	debugger *interpreter.Debugger
	stop     *interpreter.Stop
}

// sendFromQueue is to be run in a separate goroutine to listen
// on a channel for messages to send back to the client.
// It will return once the channel is closed.
func (ds *debugSession) sendFromQueue() {
	for message := range ds.sendQueue {
		ds.logger.Tracef("DAP response: %#+v", message)

		_ = dap.WriteProtocolMessage(ds.readWriter.Writer, message)
		_ = ds.readWriter.Flush()
	}
}

func (ds *debugSession) handleRequest() error {
	request, err := dap.ReadProtocolMessage(ds.readWriter.Reader)
	if err != nil {
		return err
	}

	ds.logger.Tracef("DAP request: %#+v", request)

	ds.sendWg.Add(1)
	go func() {
		ds.dispatchRequest(request)
		ds.sendWg.Done()
	}()

	return nil
}

func (ds *debugSession) send(message dap.Message) {
	ds.sendQueue <- message
}

func (ds *debugSession) dispatchRequest(request dap.Message) {
	switch request := request.(type) {
	case *dap.InitializeRequest:

		// TODO: only allow one debug session at a time

		debugger := interpreter.NewDebugger()
		ds.debugger = debugger
		ds.backend.SetDebugger(debugger)

		ds.send(&dap.InitializeResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
			Body: dap.Capabilities{
				SupportsConfigurationDoneRequest: true,
				ExceptionBreakpointFilters:       []dap.ExceptionBreakpointsFilter{},
				AdditionalModuleColumns:          []dap.ColumnDescriptor{},
				SupportedChecksumAlgorithms:      []dap.ChecksumAlgorithm{},
			},
		})

		ds.send(&dap.InitializedEvent{
			Event: newDAPEvent("initialized"),
		})

	case *dap.LaunchRequest:
		// TODO: only allow one program at a time

		var args map[string]any
		_ = json.Unmarshal(request.Arguments, &args)

		program, ok := args["program"]
		if !ok {
			ds.send(newDAPErrorResponse(
				request.Seq,
				request.Command,
				dap.ErrorMessage{
					Format:   "Missing program",
					ShowUser: true,
				},
			))
			break
		}
		code := []byte(program.(string))

		go func() {
			// TODO: add support for arguments
			// TODO: add support for transactions. requires automine

			result, err := ds.backend.ExecuteScriptAtLatestBlock(context.Background(), code, nil)

			var outputBody dap.OutputEventBody
			if err != nil {
				outputBody = dap.OutputEventBody{
					Category: "stderr",
					Output:   err.Error(),
				}
			} else {
				outputBody = dap.OutputEventBody{
					Category: "stdout",
					Output:   string(result),
				}
			}
			ds.send(&dap.OutputEvent{
				Event: newDAPEvent("output"),
				Body:  outputBody,
			})

			var exitCode int
			if err != nil {
				exitCode = 1
			}

			ds.send(&dap.ExitedEvent{
				Event: newDAPEvent("exited"),
				Body: dap.ExitedEventBody{
					ExitCode: exitCode,
				},
			})

			ds.send(&dap.TerminatedEvent{
				Event: newDAPEvent("terminated"),
			})
		}()

		ds.send(&dap.LaunchResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
		})

	case *dap.ConfigurationDoneRequest:
		ds.send(&dap.ConfigurationDoneResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
		})

	case *dap.ThreadsRequest:
		ds.send(&dap.ThreadsResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
			Body: dap.ThreadsResponseBody{
				Threads: []dap.Thread{
					{
						Id:   0,
						Name: "Emulator",
					},
				},
			},
		})

	case *dap.PauseRequest:
		ds.send(&dap.PauseResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
		})

		stop := ds.debugger.Pause()
		ds.stop = &stop

		// TODO: might be exit

		ds.send(&dap.StoppedEvent{
			Event: newDAPEvent("stopped"),
			Body: dap.StoppedEventBody{
				Reason:            "pause",
				AllThreadsStopped: true,
			},
		})

	case *dap.StackTraceRequest:

		// TODO:

		startPos := ds.stop.Statement.StartPosition()
		endPos := ds.stop.Statement.EndPosition(nil)

		ds.send(&dap.StackTraceResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
			Body: dap.StackTraceResponseBody{
				StackFrames: []dap.StackFrame{
					{
						Line:      startPos.Line,
						Column:    startPos.Column + 1,
						EndLine:   endPos.Line,
						EndColumn: endPos.Column + 1,
					},
				},
			},
		})
	case *dap.ContinueRequest:
		ds.stop = nil
		ds.debugger.Continue()

		ds.send(&dap.ContinueResponse{
			Response: newDAPSuccessResponse(request.Seq, request.Command),
			Body: dap.ContinueResponseBody{
				AllThreadsContinued: true,
			},
		})

		// NOTE: ContinuedEvent not expected:
		//   Please note: a debug adapter is not expected to send this event in response
		//   to a request that implies that execution continues, e.g. ‘launch’ or ‘continue’.
		//   It is only necessary to send a ‘continued’ event if there was no previous request that implied this.
	}
}

func newDAPEvent(event string) dap.Event {
	return dap.Event{
		ProtocolMessage: dap.ProtocolMessage{
			Seq:  0,
			Type: "event",
		},
		Event: event,
	}
}

func newDAPResponse(requestSeq int, command string, success bool) dap.Response {
	return dap.Response{
		ProtocolMessage: dap.ProtocolMessage{
			Seq:  0,
			Type: "response",
		},
		Command:    command,
		RequestSeq: requestSeq,
		Success:    success,
	}
}

func newDAPSuccessResponse(requestSeq int, command string) dap.Response {
	return newDAPResponse(requestSeq, command, true)
}

func newDAPErrorResponse(requestSeq int, command string, message dap.ErrorMessage) *dap.ErrorResponse {
	return &dap.ErrorResponse{
		Response: newDAPResponse(requestSeq, command, false),
		Body: dap.ErrorResponseBody{
			Error: message,
		},
	}
}
