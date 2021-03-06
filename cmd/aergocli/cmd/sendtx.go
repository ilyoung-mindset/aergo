/**
 *  @file
 *  @copyright defined in aergo/LICENSE.txt
 */

package cmd

import (
	"context"
	"errors"

	"github.com/aergoio/aergo/types"
	"github.com/mr-tron/base58/base58"
	"github.com/spf13/cobra"
)

var sendtxCmd = &cobra.Command{
	Use:   "sendtx",
	Short: "Send transaction",
	Args:  cobra.MinimumNArgs(0),
	RunE:  execSendTX,
}

func init() {
	rootCmd.AddCommand(sendtxCmd)
	sendtxCmd.Flags().StringVar(&from, "from", "", "Sender account address")
	sendtxCmd.MarkFlagRequired("from")
	sendtxCmd.Flags().StringVar(&to, "to", "", "Recipient account address")
	sendtxCmd.MarkFlagRequired("to")
	sendtxCmd.Flags().Uint64Var(&amount, "amount", 0, "How much in AER")
	sendtxCmd.MarkFlagRequired("amount")
}

func execSendTX(cmd *cobra.Command, args []string) error {
	account, err := types.DecodeAddress(from)
	if err != nil {
		return errors.New("Wrong address in --from flag\n" + err.Error())
	}
	recipient, err := types.DecodeAddress(to)
	if err != nil {
		return errors.New("Wrong address in --to flag\n" + err.Error())
	}
	tx := &types.Tx{Body: &types.TxBody{Account: account, Recipient: recipient, Amount: amount}}
	msg, err := client.SendTX(context.Background(), tx)
	if err != nil {
		return errors.New("Failed request to aergo sever\n" + err.Error())
	}
	cmd.Println(base58.Encode(msg.Hash), msg.Error)
	return nil
}
