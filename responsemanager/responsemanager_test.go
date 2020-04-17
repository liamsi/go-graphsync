package responsemanager

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/ipfs/go-graphsync"
	gsmsg "github.com/ipfs/go-graphsync/message"
	"github.com/ipfs/go-graphsync/responsemanager/hooks"
	"github.com/ipfs/go-graphsync/responsemanager/peerresponsemanager"
	"github.com/ipfs/go-graphsync/responsemanager/persistenceoptions"
	"github.com/ipfs/go-graphsync/selectorvalidator"
	"github.com/ipfs/go-graphsync/testutil"
	"github.com/ipfs/go-peertaskqueue/peertask"
	ipld "github.com/ipld/go-ipld-prime"
	ipldfree "github.com/ipld/go-ipld-prime/impl/free"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/stretchr/testify/require"
)

type fakeQueryQueue struct {
	popWait   sync.WaitGroup
	queriesLk sync.RWMutex
	queries   []*peertask.QueueTask
}

func (fqq *fakeQueryQueue) PushTasks(to peer.ID, tasks ...peertask.Task) {
	fqq.queriesLk.Lock()

	// This isn't quite right as the queue should deduplicate requests, but
	// it's good enough.
	for _, task := range tasks {
		fqq.queries = append(fqq.queries, peertask.NewQueueTask(task, to, time.Now()))
	}
	fqq.queriesLk.Unlock()
}

func (fqq *fakeQueryQueue) PopTasks(targetWork int) (peer.ID, []*peertask.Task, int) {
	fqq.popWait.Wait()
	fqq.queriesLk.Lock()
	defer fqq.queriesLk.Unlock()
	if len(fqq.queries) == 0 {
		return "", nil, -1
	}
	// We're not bothering to implement "work"
	task := fqq.queries[0]
	fqq.queries = fqq.queries[1:]
	return task.Target, []*peertask.Task{&task.Task}, 0
}

func (fqq *fakeQueryQueue) Remove(topic peertask.Topic, p peer.ID) {
	fqq.queriesLk.Lock()
	defer fqq.queriesLk.Unlock()
	for i, query := range fqq.queries {
		if query.Target == p && query.Topic == topic {
			fqq.queries = append(fqq.queries[:i], fqq.queries[i+1:]...)
		}
	}
}

func (fqq *fakeQueryQueue) TasksDone(to peer.ID, tasks ...*peertask.Task) {
	// We don't track active tasks so this is a no-op
}

func (fqq *fakeQueryQueue) ThawRound() {

}

type fakePeerManager struct {
	lastPeer           peer.ID
	peerResponseSender peerresponsemanager.PeerResponseSender
}

func (fpm *fakePeerManager) SenderForPeer(p peer.ID) peerresponsemanager.PeerResponseSender {
	fpm.lastPeer = p
	return fpm.peerResponseSender
}

type sentResponse struct {
	requestID graphsync.RequestID
	link      ipld.Link
	data      []byte
}

type sentExtension struct {
	requestID graphsync.RequestID
	extension graphsync.ExtensionData
}

type completedRequest struct {
	requestID graphsync.RequestID
	result    graphsync.ResponseStatusCode
}
type fakePeerResponseSender struct {
	sentResponses        chan sentResponse
	sentExtensions       chan sentExtension
	lastCompletedRequest chan completedRequest
}

func (fprs *fakePeerResponseSender) Startup()  {}
func (fprs *fakePeerResponseSender) Shutdown() {}

type fakeBlkData struct {
	link ipld.Link
	size uint64
}

func (fbd fakeBlkData) Link() ipld.Link {
	return fbd.link
}

func (fbd fakeBlkData) BlockSize() uint64 {
	return fbd.size
}

func (fbd fakeBlkData) BlockSizeOnWire() uint64 {
	return fbd.size
}

func (fprs *fakePeerResponseSender) SendResponse(
	requestID graphsync.RequestID,
	link ipld.Link,
	data []byte,
) graphsync.BlockData {
	fprs.sentResponses <- sentResponse{requestID, link, data}
	return fakeBlkData{link, uint64(len(data))}
}

func (fprs *fakePeerResponseSender) SendExtensionData(
	requestID graphsync.RequestID,
	extension graphsync.ExtensionData,
) {
	fprs.sentExtensions <- sentExtension{requestID, extension}
}

func (fprs *fakePeerResponseSender) FinishRequest(requestID graphsync.RequestID) {
	fprs.lastCompletedRequest <- completedRequest{requestID, graphsync.RequestCompletedFull}
}

func (fprs *fakePeerResponseSender) FinishWithError(requestID graphsync.RequestID, status graphsync.ResponseStatusCode) {
	fprs.lastCompletedRequest <- completedRequest{requestID, status}
}

func TestIncomingQuery(t *testing.T) {
	td := newTestData(t)
	defer td.cancel()
	blks := td.blockChain.AllBlocks()

	responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
	td.requestHooks.Register(selectorvalidator.SelectorValidator(100))
	responseManager.Startup()

	responseManager.ProcessRequests(td.ctx, td.p, td.requests)
	testutil.AssertDoesReceive(td.ctx, t, td.completedRequestChan, "Should have completed request but didn't")
	for i := 0; i < len(blks); i++ {
		var sentResponse sentResponse
		testutil.AssertReceive(td.ctx, t, td.sentResponses, &sentResponse, "did not send responses")
		k := sentResponse.link.(cidlink.Link)
		blockIndex := testutil.IndexOf(blks, k.Cid)
		require.NotEqual(t, blockIndex, -1, "sent incorrect link")
		require.Equal(t, blks[blockIndex].RawData(), sentResponse.data, "sent incorrect data")
		require.Equal(t, td.requestID, sentResponse.requestID, "has incorrect response id")
	}
}

func TestCancellationQueryInProgress(t *testing.T) {
	td := newTestData(t)
	defer td.cancel()
	blks := td.blockChain.AllBlocks()
	responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
	td.requestHooks.Register(selectorvalidator.SelectorValidator(100))
	responseManager.Startup()
	responseManager.ProcessRequests(td.ctx, td.p, td.requests)

	// read one block
	var sentResponse sentResponse
	testutil.AssertReceive(td.ctx, t, td.sentResponses, &sentResponse, "did not send response")
	k := sentResponse.link.(cidlink.Link)
	blockIndex := testutil.IndexOf(blks, k.Cid)
	require.NotEqual(t, blockIndex, -1, "sent incorrect link")
	require.Equal(t, blks[blockIndex].RawData(), sentResponse.data, "sent incorrect data")
	require.Equal(t, td.requestID, sentResponse.requestID, "has incorrect response id")

	// send a cancellation
	cancelRequests := []gsmsg.GraphSyncRequest{
		gsmsg.CancelRequest(td.requestID),
	}
	responseManager.ProcessRequests(td.ctx, td.p, cancelRequests)

	responseManager.synchronize()

	// at this point we should receive at most one more block, then traversal
	// should complete
	additionalBlocks := 0
	for {
		select {
		case <-td.ctx.Done():
			t.Fatal("should complete request before context closes")
		case sentResponse = <-td.sentResponses:
			k = sentResponse.link.(cidlink.Link)
			blockIndex = testutil.IndexOf(blks, k.Cid)
			require.NotEqual(t, blockIndex, -1, "did not send correct link")
			require.Equal(t, blks[blockIndex].RawData(), sentResponse.data, "sent incorrect data")
			require.Equal(t, td.requestID, sentResponse.requestID, "incorrect response id")
			additionalBlocks++
		case <-td.completedRequestChan:
			require.LessOrEqual(t, additionalBlocks, 1, "should send at most 1 additional block")
			return
		}
	}
}

func TestEarlyCancellation(t *testing.T) {
	td := newTestData(t)
	defer td.cancel()
	td.queryQueue.popWait.Add(1)
	responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
	responseManager.Startup()
	responseManager.ProcessRequests(td.ctx, td.p, td.requests)

	// send a cancellation
	cancelRequests := []gsmsg.GraphSyncRequest{
		gsmsg.CancelRequest(td.requestID),
	}
	responseManager.ProcessRequests(td.ctx, td.p, cancelRequests)

	responseManager.synchronize()

	// unblock popping from queue
	td.queryQueue.popWait.Done()

	timer := time.NewTimer(time.Second)
	// verify no responses processed
	testutil.AssertDoesReceiveFirst(t, timer.C, "should not process more responses", td.sentResponses, td.completedRequestChan)
}

func TestValidationAndExtensions(t *testing.T) {
	t.Run("on its own, should fail validation", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		var lastRequest completedRequest
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalFailureCode(lastRequest.result), "should terminate with failure")
	})

	t.Run("if non validating hook succeeds, does not pass validation", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()
		td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.SendExtensionData(td.extensionResponse)
		})
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		var lastRequest completedRequest
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalFailureCode(lastRequest.result), "should terminate with failure")
		var receivedExtension sentExtension
		testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
		require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")
	})

	t.Run("if validating hook succeeds, should pass validation", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()
		td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.ValidateRequest()
			hookActions.SendExtensionData(td.extensionResponse)
		})
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		var lastRequest completedRequest
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
		var receivedExtension sentExtension
		testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
		require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")
	})

	t.Run("if any hook fails, should fail", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()
		td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.ValidateRequest()
		})
		td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.SendExtensionData(td.extensionResponse)
			hookActions.TerminateWithError(errors.New("everything went to crap"))
		})
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		var lastRequest completedRequest
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalFailureCode(lastRequest.result), "should terminate with failure")
		var receivedExtension sentExtension
		testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
		require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")
	})

	t.Run("hooks can be unregistered", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()
		unregister := td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.ValidateRequest()
			hookActions.SendExtensionData(td.extensionResponse)
		})

		// hook validates request
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		var lastRequest completedRequest
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
		var receivedExtension sentExtension
		testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
		require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")

		// unregister
		unregister()

		// no same request should fail
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalFailureCode(lastRequest.result), "should terminate with failure")
	})

	t.Run("hooks can alter the loader", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		obs := make(map[ipld.Link][]byte)
		oloader, _ := testutil.NewTestStore(obs)
		responseManager := New(td.ctx, oloader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()
		// add validating hook -- so the request SHOULD succeed
		td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.ValidateRequest()
		})

		// request fails with base loader reading from block store that's missing data
		var lastRequest completedRequest
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalFailureCode(lastRequest.result), "should terminate with failure")

		err := td.peristenceOptions.Register("chainstore", td.loader)
		require.NoError(t, err)
		// register hook to use different loader
		_ = td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			if _, found := requestData.Extension(td.extensionName); found {
				hookActions.UsePersistenceOption("chainstore")
				hookActions.SendExtensionData(td.extensionResponse)
			}
		})
		// hook uses different loader that should make request succeed
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
		var receivedExtension sentExtension
		testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
		require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")
	})

	t.Run("hooks can alter the node builder chooser", func(t *testing.T) {
		td := newTestData(t)
		defer td.cancel()
		responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
		responseManager.Startup()

		customChooserCallCount := 0
		customChooser := func(ipld.Link, ipld.LinkContext) (ipld.NodeBuilder, error) {
			customChooserCallCount++
			return ipldfree.NodeBuilder(), nil
		}

		// add validating hook -- so the request SHOULD succeed
		td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			hookActions.ValidateRequest()
		})

		// with default chooser, customer chooser not called
		var lastRequest completedRequest
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
		require.Equal(t, 0, customChooserCallCount)

		// register hook to use custom chooser
		_ = td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
			if _, found := requestData.Extension(td.extensionName); found {
				hookActions.UseNodeBuilderChooser(customChooser)
				hookActions.SendExtensionData(td.extensionResponse)
			}
		})

		// verify now that request succeeds and uses custom chooser
		responseManager.ProcessRequests(td.ctx, td.p, td.requests)
		testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
		require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
		var receivedExtension sentExtension
		testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
		require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")
		require.Equal(t, 5, customChooserCallCount)
	})

	t.Run("test block hook processing", func(t *testing.T) {
		t.Run("can send extension data", func(t *testing.T) {
			td := newTestData(t)
			defer td.cancel()
			responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
			responseManager.Startup()
			td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
				hookActions.ValidateRequest()
			})
			td.blockHooks.Register(func(p peer.ID, requestData graphsync.RequestData, blockData graphsync.BlockData, hookActions graphsync.OutgoingBlockHookActions) {
				hookActions.SendExtensionData(td.extensionResponse)
			})
			responseManager.ProcessRequests(td.ctx, td.p, td.requests)
			var lastRequest completedRequest
			testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
			require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
			for i := 0; i < td.blockChainLength; i++ {
				var receivedExtension sentExtension
				testutil.AssertReceive(td.ctx, t, td.sentExtensions, &receivedExtension, "should send extension response")
				require.Equal(t, td.extensionResponse, receivedExtension.extension, "incorrect extension response sent")
			}
		})

		t.Run("can send errors", func(t *testing.T) {
			td := newTestData(t)
			defer td.cancel()
			responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
			responseManager.Startup()
			td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
				hookActions.ValidateRequest()
			})
			td.blockHooks.Register(func(p peer.ID, requestData graphsync.RequestData, blockData graphsync.BlockData, hookActions graphsync.OutgoingBlockHookActions) {
				hookActions.TerminateWithError(errors.New("failed"))
			})
			responseManager.ProcessRequests(td.ctx, td.p, td.requests)
			var lastRequest completedRequest
			testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
			require.True(t, gsmsg.IsTerminalFailureCode(lastRequest.result), "request should succeed")
		})

		t.Run("can pause/unpause", func(t *testing.T) {
			td := newTestData(t)
			defer td.cancel()
			responseManager := New(td.ctx, td.loader, td.peerManager, td.queryQueue, td.requestHooks, td.blockHooks)
			responseManager.Startup()
			td.requestHooks.Register(func(p peer.ID, requestData graphsync.RequestData, hookActions graphsync.IncomingRequestHookActions) {
				hookActions.ValidateRequest()
			})
			blkIndex := 1
			blockCount := 3
			var hasPaused bool
			td.blockHooks.Register(func(p peer.ID, requestData graphsync.RequestData, blockData graphsync.BlockData, hookActions graphsync.OutgoingBlockHookActions) {
				if blkIndex >= blockCount && !hasPaused {
					hookActions.PauseResponse()
					hasPaused = true
				}
				blkIndex++
			})
			responseManager.ProcessRequests(td.ctx, td.p, td.requests)
			timer := time.NewTimer(500 * time.Millisecond)
			testutil.AssertDoesReceiveFirst(t, timer.C, "should not complete request while paused", td.completedRequestChan)
			var sentResponses []sentResponse
		nomoreresponses:
			for {
				select {
				case sentResponse := <-td.sentResponses:
					sentResponses = append(sentResponses, sentResponse)
				default:
					break nomoreresponses
				}
			}
			require.LessOrEqual(t, len(sentResponses), blockCount)
			err := responseManager.UnpauseResponse(td.p, td.requestID)
			require.NoError(t, err)
			var lastRequest completedRequest
			testutil.AssertReceive(td.ctx, t, td.completedRequestChan, &lastRequest, "should complete request")
			require.True(t, gsmsg.IsTerminalSuccessCode(lastRequest.result), "request should succeed")
		})

	})
}

type testData struct {
	ctx                   context.Context
	cancel                func()
	blockStore            map[ipld.Link][]byte
	loader                ipld.Loader
	storer                ipld.Storer
	blockChainLength      int
	blockChain            *testutil.TestBlockChain
	completedRequestChan  chan completedRequest
	sentResponses         chan sentResponse
	sentExtensions        chan sentExtension
	peerManager           *fakePeerManager
	queryQueue            *fakeQueryQueue
	extensionData         []byte
	extensionName         graphsync.ExtensionName
	extension             graphsync.ExtensionData
	extensionResponseData []byte
	extensionResponse     graphsync.ExtensionData
	requestID             graphsync.RequestID
	requests              []gsmsg.GraphSyncRequest
	p                     peer.ID
	peristenceOptions     *persistenceoptions.PersistenceOptions
	requestHooks          *hooks.IncomingRequestHooks
	blockHooks            *hooks.OutgoingBlockHooks
}

func newTestData(t *testing.T) testData {
	ctx := context.Background()
	td := testData{}
	td.ctx, td.cancel = context.WithTimeout(ctx, 10*time.Second)

	td.blockStore = make(map[ipld.Link][]byte)
	td.loader, td.storer = testutil.NewTestStore(td.blockStore)
	td.blockChainLength = 5
	td.blockChain = testutil.SetupBlockChain(ctx, t, td.loader, td.storer, 100, td.blockChainLength)

	td.completedRequestChan = make(chan completedRequest, 1)
	td.sentResponses = make(chan sentResponse, td.blockChainLength*2)
	td.sentExtensions = make(chan sentExtension, td.blockChainLength*2)
	fprs := &fakePeerResponseSender{lastCompletedRequest: td.completedRequestChan, sentResponses: td.sentResponses, sentExtensions: td.sentExtensions}
	td.peerManager = &fakePeerManager{peerResponseSender: fprs}
	td.queryQueue = &fakeQueryQueue{}

	td.extensionData = testutil.RandomBytes(100)
	td.extensionName = graphsync.ExtensionName("AppleSauce/McGee")
	td.extension = graphsync.ExtensionData{
		Name: td.extensionName,
		Data: td.extensionData,
	}
	td.extensionResponseData = testutil.RandomBytes(100)
	td.extensionResponse = graphsync.ExtensionData{
		Name: td.extensionName,
		Data: td.extensionResponseData,
	}

	td.requestID = graphsync.RequestID(rand.Int31())
	td.requests = []gsmsg.GraphSyncRequest{
		gsmsg.NewRequest(td.requestID, td.blockChain.TipLink.(cidlink.Link).Cid, td.blockChain.Selector(), graphsync.Priority(0), td.extension),
	}
	td.p = testutil.GeneratePeers(1)[0]
	td.peristenceOptions = persistenceoptions.New()
	td.requestHooks = hooks.NewRequestHooks(td.peristenceOptions)
	td.blockHooks = hooks.NewBlockHooks()
	return td
}
