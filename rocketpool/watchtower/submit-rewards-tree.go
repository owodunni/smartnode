package watchtower

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/klauspost/compress/zstd"
	"github.com/rocket-pool/rocketpool-go/dao/trustednode"
	"github.com/rocket-pool/rocketpool-go/rewards"
	"github.com/rocket-pool/rocketpool-go/rocketpool"
	"github.com/rocket-pool/rocketpool-go/utils/eth"
	"github.com/rocket-pool/smartnode/shared/services"
	"github.com/rocket-pool/smartnode/shared/services/beacon"
	"github.com/rocket-pool/smartnode/shared/services/config"
	rprewards "github.com/rocket-pool/smartnode/shared/services/rewards"
	"github.com/rocket-pool/smartnode/shared/services/wallet"
	cfgtypes "github.com/rocket-pool/smartnode/shared/types/config"
	"github.com/rocket-pool/smartnode/shared/utils/api"
	hexutil "github.com/rocket-pool/smartnode/shared/utils/hex"
	"github.com/rocket-pool/smartnode/shared/utils/log"
	"github.com/urfave/cli"
	"github.com/web3-storage/go-w3s-client"
)

// Submit rewards Merkle Tree task
type submitRewardsTree struct {
	c                *cli.Context
	log              log.ColorLogger
	errLog           log.ColorLogger
	cfg              *config.RocketPoolConfig
	w                *wallet.Wallet
	rp               *rocketpool.RocketPool
	ec               rocketpool.ExecutionClient
	bc               beacon.Client
	lock             *sync.Mutex
	isRunning        bool
	generationPrefix string
}

// Create submit rewards Merkle Tree task
func newSubmitRewardsTree(c *cli.Context, logger log.ColorLogger, errorLogger log.ColorLogger) (*submitRewardsTree, error) {

	// Get services
	cfg, err := services.GetConfig(c)
	if err != nil {
		return nil, err
	}
	w, err := services.GetWallet(c)
	if err != nil {
		return nil, err
	}
	ec, err := services.GetEthClient(c)
	if err != nil {
		return nil, err
	}
	bc, err := services.GetBeaconClient(c)
	if err != nil {
		return nil, err
	}
	rp, err := services.GetRocketPool(c)
	if err != nil {
		return nil, err
	}

	lock := &sync.Mutex{}
	generator := &submitRewardsTree{
		c:                c,
		log:              logger,
		errLog:           errorLogger,
		cfg:              cfg,
		ec:               ec,
		bc:               bc,
		w:                w,
		rp:               rp,
		lock:             lock,
		isRunning:        false,
		generationPrefix: "[Merkle Tree]",
	}

	return generator, nil
}

// Submit rewards Merkle Tree
func (t *submitRewardsTree) run() error {

	// Wait for clients to sync
	if err := services.WaitEthClientSynced(t.c, true); err != nil {
		return err
	}
	if err := services.WaitBeaconClientSynced(t.c, true); err != nil {
		return err
	}

	// Get node account
	nodeAccount, err := t.w.GetNodeAccount()
	if err != nil {
		return err
	}

	// Check node trusted status
	nodeTrusted, err := trustednode.GetMemberExists(t.rp, nodeAccount.Address, nil)
	if err != nil {
		return err
	}
	if !nodeTrusted && t.cfg.Smartnode.RewardsTreeMode.Value.(cfgtypes.RewardsMode) != cfgtypes.RewardsMode_Generate {
		return nil
	}

	// Log
	t.log.Println("Checking for rewards checkpoint...")

	// Check if a rewards interval has passed and needs to be calculated
	startTime, err := rewards.GetClaimIntervalTimeStart(t.rp, nil)
	if err != nil {
		return fmt.Errorf("error getting claim interval start time: %w", err)
	}
	intervalTime, err := rewards.GetClaimIntervalTime(t.rp, nil)
	if err != nil {
		return fmt.Errorf("error getting claim interval time: %w", err)
	}

	// Calculate the end time, which is the number of intervals that have gone by since the current one's start
	latestBlockHeader, err := t.ec.HeaderByNumber(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("error getting latest block header: %w", err)
	}
	latestBlockTime := time.Unix(int64(latestBlockHeader.Time), 0)
	timeSinceStart := latestBlockTime.Sub(startTime)
	intervalsPassed := timeSinceStart / intervalTime
	endTime := startTime.Add(intervalTime * intervalsPassed)
	if intervalsPassed == 0 {
		return nil
	}

	// Get the block and timestamp of the consensus block that best matches the end time
	snapshotBeaconBlock, elBlockNumber, nextIntervalEpochTime, err := t.getSnapshotConsensusBlock(endTime)
	if err != nil {
		return err
	}

	// Get the number of the EL block matching the CL snapshot block
	var snapshotElBlockHeader *types.Header
	if elBlockNumber == 0 {
		// No EL data so the Merge hasn't happened yet, figure out the EL block based on the Epoch ending time
		snapshotElBlockHeader, err = rprewards.GetELBlockHeaderForTime(nextIntervalEpochTime, t.ec)
	} else {
		snapshotElBlockHeader, err = t.ec.HeaderByNumber(context.Background(), big.NewInt(int64(elBlockNumber)))
	}
	if err != nil {
		return err
	}
	elBlockIndex := snapshotElBlockHeader.Number.Uint64()

	// Get the current interval
	currentIndexBig, err := rewards.GetRewardIndex(t.rp, nil)
	if err != nil {
		return err
	}
	currentIndex := currentIndexBig.Uint64()

	// Check if rewards generation is already running
	t.lock.Lock()
	if t.isRunning {
		t.log.Println("Tree generation is already running in the background.")
		t.lock.Unlock()
		return nil
	}
	t.lock.Unlock()

	// Get the expected file paths
	rewardsTreePath := t.cfg.Smartnode.GetRewardsTreePath(currentIndex, true)
	compressedRewardsTreePath := rewardsTreePath + config.RewardsTreeIpfsExtension
	minipoolPerformancePath := t.cfg.Smartnode.GetMinipoolPerformancePath(currentIndex, true)
	compressedMinipoolPerformancePath := minipoolPerformancePath + config.RewardsTreeIpfsExtension

	// Check if we can reuse an existing file fir this interval
	if t.isExistingFileValid(rewardsTreePath, uint64(intervalsPassed)) {
		if !nodeTrusted {
			t.log.Printlnf("Merkle rewards tree for interval %d already exists at %s.", currentIndex, rewardsTreePath)
			return nil
		}

		// Return if this node has already submitted the tree for the current interval and there's a file present
		hasSubmitted, err := t.hasSubmittedTree(nodeAccount.Address, currentIndexBig)
		if err != nil {
			return fmt.Errorf("error checking if Merkle tree submission has already been processed: %w", err)
		}
		if hasSubmitted {
			return nil
		}

		t.log.Printlnf("Merkle rewards tree for interval %d already exists at %s, attempting to resubmit...", currentIndex, rewardsTreePath)

		// Deserialize the file
		wrapperBytes, err := ioutil.ReadFile(rewardsTreePath)
		if err != nil {
			return fmt.Errorf("Error reading rewards tree file: %w", err)
		}

		proofWrapper := new(rprewards.RewardsFile)
		err = json.Unmarshal(wrapperBytes, proofWrapper)
		if err != nil {
			return fmt.Errorf("Error deserializing rewards tree file: %w", err)
		}

		// Upload the file
		cid, err := t.uploadFileToWeb3Storage(wrapperBytes, compressedRewardsTreePath, "compressed rewards tree")
		if err != nil {
			return fmt.Errorf("Error uploading Merkle tree to Web3.Storage: %w", err)
		}
		t.log.Printlnf("Uploaded Merkle tree with CID %s", cid)

		// Submit to the contracts
		err = t.submitRewardsSnapshot(currentIndexBig, snapshotBeaconBlock, elBlockIndex, proofWrapper, cid, big.NewInt(int64(intervalsPassed)))
		if err != nil {
			return fmt.Errorf("Error submitting rewards snapshot: %w", err)
		}

		t.log.Printlnf("Successfully submitted rewards snapshot for interval %d.", currentIndex)
		return nil
	}

	// Generate the tree
	t.generateTree(intervalsPassed, nodeTrusted, currentIndex, snapshotBeaconBlock, elBlockIndex, startTime, endTime, snapshotElBlockHeader, rewardsTreePath, compressedRewardsTreePath, minipoolPerformancePath, compressedMinipoolPerformancePath)

	// Done
	return nil

}

func (t *submitRewardsTree) handleError(err error) {
	t.errLog.Println(fmt.Errorf("%s %w", t.generationPrefix, err))
	t.errLog.Println("*** Rewards tree generation failed. ***")
	t.lock.Lock()
	t.isRunning = false
	t.lock.Unlock()
}

// Print a message from the tree generation goroutine
func (t *submitRewardsTree) printMessage(message string) {
	t.log.Printlnf("%s %s", t.generationPrefix, message)
}

// Checks to see if an existing rewards file is still valid
func (t *submitRewardsTree) isExistingFileValid(rewardsTreePath string, intervalsPassed uint64) bool {

	_, err := os.Stat(rewardsTreePath)
	if !os.IsNotExist(err) {
		// The file already exists, attempt to read it
		var proofWrapper rprewards.RewardsFile
		fileBytes, err := ioutil.ReadFile(rewardsTreePath)
		if err != nil {
			t.log.Printlnf("WARNING: failed to read %s: %s\nRegenerating file...\n", rewardsTreePath, err.Error())
			return false
		} else {
			err = json.Unmarshal(fileBytes, &proofWrapper)
			if err != nil {
				t.log.Printlnf("WARNING: failed to deserialize %s: %s\nRegenerating file...\n", rewardsTreePath, err.Error())
				return false
			}
		}

		// Compare the number of intervals in it with the current number of intervals
		if proofWrapper.IntervalsPassed != intervalsPassed {
			t.log.Printlnf("Existing file for interval %d had %d intervals passed but %d have passed now, regenerating file...\n", proofWrapper.Index, proofWrapper.IntervalsPassed, intervalsPassed)
			return false
		}

		// File's good and it has the same number of intervals passed, so use it
		return true
	}

	return false

}

// Kick off the tree generation goroutine
func (t *submitRewardsTree) generateTree(intervalsPassed time.Duration, nodeTrusted bool, currentIndex uint64, snapshotBeaconBlock uint64, elBlockIndex uint64, startTime time.Time, endTime time.Time, snapshotElBlockHeader *types.Header, rewardsTreePath string, compressedRewardsTreePath string, minipoolPerformancePath string, compressedMinipoolPerformancePath string) {

	go func() {
		t.lock.Lock()
		t.isRunning = true
		t.lock.Unlock()

		client := t.rp

		// Try getting the rETH address as a canary to see if the block is available
		opts := &bind.CallOpts{
			BlockNumber: snapshotElBlockHeader.Number,
		}
		address, err := client.RocketStorage.GetAddress(opts, crypto.Keccak256Hash([]byte("contract.addressrocketTokenRETH")))
		if err != nil {
			errMessage := err.Error()
			t.printMessage(fmt.Sprintf("Error getting state for block %d: %s", snapshotElBlockHeader.Number.Uint64(), errMessage))
			if strings.Contains(errMessage, "missing trie node") || // Geth
				strings.Contains(errMessage, "No state available for block") || // Nethermind
				strings.Contains(errMessage, "Internal error") { // Besu

				// The state was missing so fall back to the archive node
				archiveEcUrl := t.cfg.Smartnode.ArchiveECUrl.Value.(string)
				if archiveEcUrl != "" {
					t.printMessage(fmt.Sprintf("Primary EC cannot retrieve state for historical block %d, using archive EC [%s]", snapshotElBlockHeader.Number.Uint64(), archiveEcUrl))
					ec, err := ethclient.Dial(archiveEcUrl)
					if err != nil {
						t.handleError(fmt.Errorf("Error connecting to archive EC: %w", err))
						return
					}
					client, err = rocketpool.NewRocketPool(ec, common.HexToAddress(t.cfg.Smartnode.GetStorageAddress()))
					if err != nil {
						t.handleError(fmt.Errorf("%s Error creating Rocket Pool client connected to archive EC: %w", err))
						return
					}

					// Get the rETH address from the archive EC
					address, err = client.RocketStorage.GetAddress(opts, crypto.Keccak256Hash([]byte("contract.addressrocketTokenRETH")))
					if err != nil {
						t.handleError(fmt.Errorf("%s Error verifying rETH address with Archive EC: %w", err))
						return
					}
				} else {
					// No archive node specified
					t.handleError(fmt.Errorf("***ERROR*** Primary EC cannot retrieve state for historical block %d and the Archive EC is not specified.", snapshotElBlockHeader.Number.Uint64()))
					return
				}

			}
		}

		// Sanity check the rETH address to make sure the client is working right
		if address != t.cfg.Smartnode.GetRethAddress() {
			t.handleError(fmt.Errorf("***ERROR*** Your Primary EC provided %s as the rETH address, but it should have been %s!", address.Hex(), t.cfg.Smartnode.GetRethAddress().Hex()))
			return
		}

		// Generate the tree
		err = t.generateTreeImpl(client, intervalsPassed, nodeTrusted, currentIndex, snapshotBeaconBlock, elBlockIndex, startTime, endTime, snapshotElBlockHeader, rewardsTreePath, compressedRewardsTreePath, minipoolPerformancePath, compressedMinipoolPerformancePath)
		if err != nil {
			t.handleError(err)
		}

		t.lock.Lock()
		t.isRunning = false
		t.lock.Unlock()
	}()

}

func (t *submitRewardsTree) generateTreeImpl(rp *rocketpool.RocketPool, intervalsPassed time.Duration, nodeTrusted bool, currentIndex uint64, snapshotBeaconBlock uint64, elBlockIndex uint64, startTime time.Time, endTime time.Time, snapshotElBlockHeader *types.Header, rewardsTreePath string, compressedRewardsTreePath string, minipoolPerformancePath string, compressedMinipoolPerformancePath string) error {

	// Log
	if uint64(intervalsPassed) > 1 {
		t.log.Printlnf("WARNING: %d intervals have passed since the last rewards checkpoint was submitted! Rolling them into one...", uint64(intervalsPassed))
	}
	t.log.Printlnf("Rewards checkpoint has passed, starting Merkle tree generation for interval %d in the background.\n%s Snapshot Beacon block = %d, EL block = %d, running from %s to %s", currentIndex, t.generationPrefix, snapshotBeaconBlock, elBlockIndex, startTime, endTime)

	// Generate the rewards file
	rewardsFile := rprewards.NewRewardsFile(t.log, t.generationPrefix, currentIndex, startTime, endTime, snapshotBeaconBlock, snapshotElBlockHeader, uint64(intervalsPassed))
	err := rewardsFile.GenerateTree(rp, t.cfg, t.bc)
	if err != nil {
		return fmt.Errorf("Error generating Merkle tree: %w", err)
	}
	for address, network := range rewardsFile.InvalidNetworkNodes {
		t.printMessage(fmt.Sprintf("WARNING: Node %s has invalid network %d assigned! Using 0 (mainnet) instead.", address.Hex(), network))
	}

	// Serialize the minipool performance file
	minipoolPerformanceBytes, err := json.Marshal(rewardsFile.MinipoolPerformanceFile)
	if err != nil {
		return fmt.Errorf("Error serializing minipool performance file into JSON: %w", err)
	}

	// Write it to disk
	err = ioutil.WriteFile(minipoolPerformancePath, minipoolPerformanceBytes, 0644)
	if err != nil {
		return fmt.Errorf("Error saving minipool performance file to %s: %w", minipoolPerformancePath, err)
	}

	// Upload it if this is an Oracle DAO node
	if nodeTrusted {
		t.printMessage("Uploading minipool performance file to Web3.Storage...")
		minipoolPerformanceCid, err := t.uploadFileToWeb3Storage(minipoolPerformanceBytes, compressedMinipoolPerformancePath, "compressed minipool performance")
		if err != nil {
			return fmt.Errorf("Error uploading minipool performance file to Web3.Storage: %w", err)
		}
		t.printMessage(fmt.Sprintf("Uploaded minipool performance file with CID %s", minipoolPerformanceCid))
		rewardsFile.MinipoolPerformanceFileCID = minipoolPerformanceCid
	} else {
		t.printMessage("Saved minipool performance file.")
		rewardsFile.MinipoolPerformanceFileCID = "---"
	}

	// Serialize the rewards tree to JSON
	wrapperBytes, err := json.Marshal(rewardsFile)
	if err != nil {
		return fmt.Errorf("Error serializing proof wrapper into JSON: %w", err)
	}
	t.printMessage("Generation complete! Saving tree...")

	// Write the rewards tree to disk
	err = ioutil.WriteFile(rewardsTreePath, wrapperBytes, 0644)
	if err != nil {
		return fmt.Errorf("Error saving rewards tree file to %s: %w", rewardsTreePath, err)
	}

	// Only do the upload and submission process if this is an Oracle DAO node
	if nodeTrusted {
		// Upload the rewards tree file
		t.printMessage("Uploading to Web3.Storage and submitting results to the contracts...")
		cid, err := t.uploadFileToWeb3Storage(wrapperBytes, compressedRewardsTreePath, "compressed rewards tree")
		if err != nil {
			return fmt.Errorf("Error uploading Merkle tree to Web3.Storage: %w", err)
		}
		t.printMessage(fmt.Sprintf("Uploaded Merkle tree with CID %s", cid))

		// Submit to the contracts
		err = t.submitRewardsSnapshot(big.NewInt(int64(currentIndex)), snapshotBeaconBlock, elBlockIndex, rewardsFile, cid, big.NewInt(int64(intervalsPassed)))
		if err != nil {
			return fmt.Errorf("Error submitting rewards snapshot: %w", err)
		}

		t.printMessage(fmt.Sprintf("Successfully submitted rewards snapshot for interval %d.", currentIndex))
	} else {
		t.printMessage(fmt.Sprintf("Successfully generated rewards snapshot for interval %d.", currentIndex))
	}

	return nil

}

// Submit rewards info to the contracts
func (t *submitRewardsTree) submitRewardsSnapshot(index *big.Int, consensusBlock uint64, executionBlock uint64, rewardsFile *rprewards.RewardsFile, cid string, intervalsPassed *big.Int) error {

	treeRootBytes, err := hex.DecodeString(hexutil.RemovePrefix(rewardsFile.MerkleRoot))
	if err != nil {
		return fmt.Errorf("Error decoding merkle root: %w", err)
	}
	treeRoot := common.BytesToHash(treeRootBytes)

	// Create the arrays of rewards per network
	collateralRplRewards := []*big.Int{}
	oDaoRplRewards := []*big.Int{}
	smoothingPoolEthRewards := []*big.Int{}

	// Create the total rewards for each network
	network := uint64(0)
	for {
		networkRewards, exists := rewardsFile.NetworkRewards[network]
		if !exists {
			break
		}

		collateralRplRewards = append(collateralRplRewards, &networkRewards.CollateralRpl.Int)
		oDaoRplRewards = append(oDaoRplRewards, &networkRewards.OracleDaoRpl.Int)
		smoothingPoolEthRewards = append(smoothingPoolEthRewards, &networkRewards.SmoothingPoolEth.Int)

		network++
	}

	// Get transactor
	opts, err := t.w.GetNodeAccountTransactor()
	if err != nil {
		return err
	}

	// Create the submission
	submission := rewards.RewardSubmission{
		RewardIndex:     index,
		ExecutionBlock:  big.NewInt(0).SetUint64(executionBlock),
		ConsensusBlock:  big.NewInt(0).SetUint64(consensusBlock),
		MerkleRoot:      treeRoot,
		MerkleTreeCID:   cid,
		IntervalsPassed: intervalsPassed,
		TreasuryRPL:     &rewardsFile.TotalRewards.ProtocolDaoRpl.Int,
		NodeRPL:         collateralRplRewards,
		TrustedNodeRPL:  oDaoRplRewards,
		NodeETH:         smoothingPoolEthRewards,
		UserETH:         &rewardsFile.TotalRewards.PoolStakerSmoothingPoolEth.Int,
	}

	// Get the gas limit
	gasInfo, err := rewards.EstimateSubmitRewardSnapshotGas(t.rp, submission, opts)
	if err != nil {
		return fmt.Errorf("Could not estimate the gas required to submit the rewards tree: %w", err)
	}

	// Print the gas info
	maxFee := eth.GweiToWei(WatchtowerMaxFee)
	if !api.PrintAndCheckGasInfo(gasInfo, false, 0, t.log, maxFee, 0) {
		return nil
	}

	opts.GasFeeCap = maxFee
	opts.GasTipCap = eth.GweiToWei(WatchtowerMaxPriorityFee)
	opts.GasLimit = gasInfo.SafeGasLimit

	// Submit RPL price
	hash, err := rewards.SubmitRewardSnapshot(t.rp, submission, opts)
	if err != nil {
		return err
	}

	// Print TX info and wait for it to be mined
	err = api.PrintAndWaitForTransaction(t.cfg, hash, t.rp.Client, t.log)
	if err != nil {
		return err
	}

	// Return
	return nil
}

// Compress and upload a file to Web3.Storage and get the CID for it
func (t *submitRewardsTree) uploadFileToWeb3Storage(wrapperBytes []byte, compressedPath string, description string) (string, error) {

	// Get the API token
	apiToken := t.cfg.Smartnode.Web3StorageApiToken.Value.(string)
	if apiToken == "" {
		return "", fmt.Errorf("***ERROR***\nYou have not configured your Web3.Storage API token yet, so you cannot submit Merkle rewards trees.\nPlease get an API token from https://web3.storage and enter it in the Smartnode section of the `service config` TUI (or use `--smartnode-web3StorageApiToken` if you configure your system headlessly).")
	}

	// Create the client
	w3sClient, err := w3s.NewClient(w3s.WithToken(apiToken))
	if err != nil {
		return "", fmt.Errorf("Error creating new Web3.Storage client: %w", err)
	}

	// Compress the file
	encoder, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	compressedBytes := encoder.EncodeAll(wrapperBytes, make([]byte, 0, len(wrapperBytes)))

	// Create the compressed tree file
	compressedFile, err := os.Create(compressedPath)
	if err != nil {
		return "", fmt.Errorf("Error creating %s file [%s]: %w", description, compressedPath, err)
	}
	defer compressedFile.Close()

	// Write the compressed data to the file
	_, err = compressedFile.Write(compressedBytes)
	if err != nil {
		return "", fmt.Errorf("Error writing %s to %s: %w", description, compressedPath, err)
	}

	// Rewind it to the start
	compressedFile.Seek(0, 0)

	// Upload it
	cid, err := w3sClient.Put(context.Background(), compressedFile)
	if err != nil {
		return "", fmt.Errorf("Error uploading %s: %w", description, err)
	}

	return cid.String(), nil

}

// Get the first finalized, successful consensus block that occurred after the given target time
func (t *submitRewardsTree) getSnapshotConsensusBlock(endTime time.Time) (uint64, uint64, time.Time, error) {

	// Get the config
	eth2Config, err := t.bc.GetEth2Config()
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("Error getting Beacon config: %w", err)
	}

	// Get the beacon head
	beaconHead, err := t.bc.GetBeaconHead()
	if err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("Error getting Beacon head: %w", err)
	}

	// Get the target block number
	genesisTime := time.Unix(int64(eth2Config.GenesisTime), 0)
	totalTimespan := endTime.Sub(genesisTime)
	targetSlot := uint64(math.Ceil(totalTimespan.Seconds() / float64(eth2Config.SecondsPerSlot)))
	targetSlotEpoch := targetSlot / eth2Config.SlotsPerEpoch
	targetSlot = targetSlotEpoch*eth2Config.SlotsPerEpoch + (eth2Config.SlotsPerEpoch - 1) // The target slot becomes the last one in the Epoch
	requiredEpoch := targetSlotEpoch + 1                                                   // The smoothing pool requires 1 epoch beyond the target to be finalized, to check for late attestations

	// Check if the required epoch is finalized yet
	if beaconHead.FinalizedEpoch < requiredEpoch {
		return 0, 0, time.Time{}, fmt.Errorf("Snapshot end time = %s, slot (epoch) = %d (%d)... waiting until epoch %d is finalized (currently %d).", endTime, targetSlot, targetSlotEpoch, requiredEpoch, beaconHead.FinalizedEpoch)
	}

	// Get the first successful block
	for {
		// Try to get the current block
		block, exists, err := t.bc.GetBeaconBlock(fmt.Sprint(targetSlot))
		if err != nil {
			return 0, 0, time.Time{}, fmt.Errorf("Error getting Beacon block %d: %w", targetSlot, err)
		}

		// If the block was missing, try the previous one
		if !exists {
			t.log.Printlnf("Slot %d was missing, trying the previous one...", targetSlot)
			targetSlot--
		} else {
			// Ok, we have the first proposed finalized block - this is the one to use for the snapshot!
			blockTime := genesisTime.Add(time.Duration(requiredEpoch*eth2Config.SecondsPerEpoch) * time.Second)
			return targetSlot, block.ExecutionBlockNumber, blockTime, nil
		}
	}

}

// Check whether the rewards tree for the current interval been submitted by the node
func (t *submitRewardsTree) hasSubmittedTree(nodeAddress common.Address, index *big.Int) (bool, error) {
	indexBuffer := make([]byte, 32)
	index.FillBytes(indexBuffer)
	return t.rp.RocketStorage.GetBool(nil, crypto.Keccak256Hash([]byte("rewards.snapshot.submitted.node"), nodeAddress.Bytes(), indexBuffer))
}
