package node

import (
    "bytes"
    "errors"
    "fmt"
    "math/big"

    "github.com/ethereum/go-ethereum/common"
    "github.com/urfave/cli"

    "github.com/rocket-pool/smartnode-cli/rocketpool/utils/eth"
)


// Withdraw resources from the node
func withdrawFromNode(c *cli.Context, amount float64, unit string) error {

    // Command setup
    if message, err := setup(c, []string{"rocketNodeAPI"}, []string{"rocketNodeContract"}, true); message != "" {
        fmt.Println(message)
        return nil
    } else if err != nil {
        return err
    }

    // Check node is registered (contract exists)
    nodeContractAddress := new(common.Address)
    if err := cm.Contracts["rocketNodeAPI"].Call(nil, nodeContractAddress, "getContract", am.GetNodeAccount().Address); err != nil {
        return errors.New("Error checking node registration: " + err.Error())
    } else if bytes.Equal(nodeContractAddress.Bytes(), make([]byte, common.AddressLength)) {
        fmt.Println("Node is not registered with Rocket Pool, please register with `rocketpool node register`")
        return nil
    }

    // Initialise node contract
    nodeContract, err := cm.NewContract(nodeContractAddress, "rocketNodeContract")
    if err != nil {
        return errors.New("Error initialising node contract: " + err.Error())
    }

    // Convert withdrawal amount to wei
    amountWei := eth.EthToWei(amount)

    // Get contract method names
    var balanceMethod string
    var withdrawMethod string
    switch unit {
        case "ETH":
            balanceMethod = "getBalanceETH"
            withdrawMethod = "withdrawEther"
        case "RPL":
            balanceMethod = "getBalanceRPL"
            withdrawMethod = "withdrawRPL"
    }

    // Check withdrawal amount is available
    balanceWei := new(*big.Int)
    if err := nodeContract.Call(nil, balanceWei, balanceMethod); err != nil {
        return errors.New("Error retrieving node balance: " + err.Error())
    } else if amountWei.Cmp(*balanceWei) > 0 {
        fmt.Println("Withdrawal amount exceeds available balance on node contract")
        return nil
    }

    // Withdraw amount
    if txor, err := am.GetNodeAccountTransactor(); err != nil {
        return err
    } else {
        txor.GasLimit = 100000 // Gas estimates on this method are incorrect
        if _, err := nodeContract.Transact(txor, withdrawMethod, amountWei); err != nil {
            return errors.New("Error withdrawing from node contract: " + err.Error())
        }
    }

    // Log & return
    fmt.Println(fmt.Sprintf("Successfully withdrew %.2f %s from node contract to account", amount, unit))
    return nil

}

