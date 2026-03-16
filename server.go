//go:build !agent

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sync"
)

type server struct {
	store *store
	pool  *connPool
	agent remoteAgent
	ctx   context.Context
}

func (s *server) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.ctx = ctx

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, platformSignals()...)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var wg sync.WaitGroup
	responses := make(chan jsonRPCResponse, 100)

	go func() {
		for resp := range responses {
			data, err := json.Marshal(resp)
			if err != nil {
				fmt.Fprintf(os.Stderr, "marshal response: %v\n", err)
				continue
			}
			fmt.Println(string(data))
		}
	}()

	lineChan := make(chan string)
	go func() {
		for scanner.Scan() {
			lineChan <- scanner.Text()
		}
		close(lineChan)
	}()

	for {
		select {
		case sig := <-sigChan:
			fmt.Fprintf(os.Stderr, "received %v, shutting down\n", sig)
			cancel()
			if s.pool != nil {
				s.pool.closeAll()
			}
			go func() {
				<-sigChan
				os.Exit(1)
			}()
			wg.Wait()
			close(responses)
			return
		case line, ok := <-lineChan:
			if !ok {
				cancel()
				if s.pool != nil {
					s.pool.closeAll()
				}
				wg.Wait()
				close(responses)
				return
			}
			if line == "" {
				continue
			}

			var req jsonRPCRequest
			if err := json.Unmarshal([]byte(line), &req); err != nil {
				responses <- jsonRPCResponse{
					JSONRPC: "2.0",
					Error:   &rpcError{Code: -32700, Message: "Parse error"},
				}
				continue
			}

			wg.Add(1)
			go func(r jsonRPCRequest) {
				defer wg.Done()
				s.handleRequest(&r, responses)
			}(req)
		}
	}
}

func (s *server) handleRequest(req *jsonRPCRequest, out chan<- jsonRPCResponse) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(req, out)
	case "notifications/initialized":
		// no-op
	case "tools/list":
		s.handleToolsList(req, out)
	case "tools/call":
		s.handleToolsCall(req, out)
	default:
		if req.ID != nil {
			out <- jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error:   &rpcError{Code: -32601, Message: "Method not found"},
			}
		}
	}
}

func (s *server) handleInitialize(req *jsonRPCRequest, out chan<- jsonRPCResponse) {
	out <- jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "devops",
				"version": Version,
			},
		},
	}
}

func (s *server) handleToolsList(req *jsonRPCRequest, out chan<- jsonRPCResponse) {
	out <- jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  map[string]any{"tools": toolDefinitions()},
	}
}

func (s *server) handleToolsCall(req *jsonRPCRequest, out chan<- jsonRPCResponse) {
	var params toolCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		out <- jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: "Invalid params"},
		}
		return
	}

	var result string
	var isError bool

	switch params.Name {
	case "devops_version":
		result, isError = Version, false
	case "devops_list":
		result, isError = s.devopsList(params.Arguments)
	case "devops_add":
		result, isError = s.devopsAdd(params.Arguments)
	case "devops_remove":
		result, isError = s.devopsRemove(params.Arguments)
	case "devops_update":
		result, isError = s.devopsUpdate(params.Arguments)
	case "devops_import":
		result, isError = s.devopsImport(params.Arguments)
	case "devops_status":
		result, isError = s.devopsStatus(params.Arguments)
	case "devops_deploy":
		result, isError = s.devopsDeploy(params.Arguments)
	case "devops_restart":
		result, isError = s.devopsRestart(params.Arguments)
	case "devops_stop":
		result, isError = s.devopsStop(params.Arguments)
	case "devops_logs":
		result, isError = s.devopsLogs(params.Arguments)
	case "devops_exec":
		result, isError = s.devopsExec(params.Arguments)
	case "devops_health":
		result, isError = s.devopsHealth(params.Arguments)
	case "devops_bootstrap":
		result, isError = s.devopsBootstrap(params.Arguments)
	case "devops_filter_add":
		result, isError = s.devopsFilterAdd(params.Arguments)
	case "devops_filter_list":
		result, isError = s.devopsFilterList(params.Arguments)
	case "devops_filter_remove":
		result, isError = s.devopsFilterRemove(params.Arguments)
	case "devops_filter_sync":
		result, isError = s.devopsFilterSync(params.Arguments)
	default:
		out <- jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: "Unknown tool"},
		}
		return
	}

	out <- jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": result,
				},
			},
			"isError": isError,
		},
	}
}
