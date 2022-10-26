package zcncore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/0chain/errors"
	"github.com/0chain/gosdk/core/transaction"
	"github.com/0chain/gosdk/core/util"
	"github.com/0chain/gosdk/core/zcncrypto"
)

type TransactionWithAuth struct {
	*Transaction
}

func (ta *TransactionWithAuth) Hash() string {
	return ta.txnHash
}

func (ta *TransactionWithAuth) SetTransactionNonce(txnNonce int64) error {
	return ta.SetTransactionNonce(txnNonce)
}

func newTransactionWithAuth(cb TransactionCallback, txnFee uint64, nonce int64) (*TransactionWithAuth, error) {
	ta := &TransactionWithAuth{}
	var err error
	ta.Transaction, err = newTransaction(cb, txnFee, nonce)
	return ta, err
}

func (ta *TransactionWithAuth) getAuthorize() (*transaction.Transaction, error) {
	ta.txn.PublicKey = _config.wallet.Keys[0].PublicKey
	err := ta.txn.ComputeHashAndSign(SignFn)
	if err != nil {
		return nil, errors.Wrap(err, "signing error.")
	}
	req, err := util.NewHTTPPostRequest(_config.authUrl+"/transaction", ta.txn)
	if err != nil {
		return nil, errors.Wrap(err, "new post request failed for auth")
	}
	res, err := req.Post()
	if err != nil {
		return nil, errNetwork
	}
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusUnauthorized {
			return nil, errUserRejected
		}
		return nil, errors.New(strconv.Itoa(res.StatusCode), fmt.Sprintf("auth error: %v. %v", res.Status, res.Body))
	}
	var txnResp transaction.Transaction
	err = json.Unmarshal([]byte(res.Body), &txnResp)
	if err != nil {
		return nil, errors.Wrap(err, "invalid json on auth response.")
	}
	logging.Debug(txnResp)
	// Verify the signature on the result
	ok, err := txnResp.VerifyTransaction(verifyFn)
	if err != nil {
		logging.Error("verification failed for txn from auth", err.Error())
		return nil, errAuthVerifyFailed
	}
	if !ok {
		ta.completeTxn(StatusAuthVerifyFailed, "", errAuthVerifyFailed)
		return nil, errAuthVerifyFailed
	}
	return &txnResp, nil
}

func (ta *TransactionWithAuth) completeTxn(status int, out string, err error) {
	// do error code translation
	if status != StatusSuccess {
		switch err {
		case errNetwork:
			status = StatusNetworkError
		case errUserRejected:
			status = StatusRejectedByUser
		case errAuthVerifyFailed:
			status = StatusAuthVerifyFailed
		case errAuthTimeout:
			status = StatusAuthTimeout
		}
	}
	ta.completeTxn(status, out, err)
}

func (ta *TransactionWithAuth) SetTransactionCallback(cb TransactionCallback) error {
	return ta.SetTransactionCallback(cb)
}

func verifyFn(signature, msgHash, publicKey string) (bool, error) {
	v := zcncrypto.NewSignatureScheme(_config.chain.SignatureScheme)
	v.SetPublicKey(publicKey)
	ok, err := v.Verify(signature, msgHash)
	if err != nil || !ok {
		return false, errors.New("", `{"error": "signature_mismatch"}`)
	}
	return true, nil
}

func (ta *TransactionWithAuth) sign(otherSig string) error {
	ta.txn.ComputeHashData()
	sig := zcncrypto.NewSignatureScheme(_config.chain.SignatureScheme)
	sig.SetPrivateKey(_config.wallet.Keys[0].PrivateKey)
	var err error
	ta.txn.Signature, err = sig.Add(otherSig, ta.txn.Hash)
	return err
}

func (ta *TransactionWithAuth) submitTxn() {
	nonce := ta.txn.TransactionNonce
	if nonce < 1 {
		nonce = transaction.Cache.GetNextNonce(ta.txn.ClientID)
	} else {
		transaction.Cache.Set(ta.txn.ClientID, nonce)
	}
	ta.txn.TransactionNonce = nonce

	authTxn, err := ta.getAuthorize()
	if err != nil {
		logging.Error("get auth error for send.", err.Error())
		ta.completeTxn(StatusAuthError, "", err)
		return
	}
	// Authorized by user. Give callback to app.
	if ta.txnCb != nil {
		ta.txnCb.OnAuthComplete(ta.Transaction, StatusSuccess)
	}
	// Use the timestamp from auth and sign
	ta.txn.CreationDate = authTxn.CreationDate
	err = ta.sign(authTxn.Signature)
	if err != nil {
		ta.completeTxn(StatusError, "", errAddSignature)
	}
	ta.submitTxn()
}

func (ta *TransactionWithAuth) StoreData(data string) error {
	go func() {
		ta.txn.TransactionType = transaction.TxnTypeData
		ta.txn.TransactionData = data
		ta.submitTxn()
	}()
	return nil
}

// ExecuteFaucetSCWallet impements the Faucet Smart contract for a given wallet
func (ta *TransactionWithAuth) ExecuteFaucetSCWallet(walletStr string, methodName string, input []byte) error {
	w, err := ta.createFaucetSCWallet(walletStr, methodName, input)
	if err != nil {
		return err
	}
	go func() {
		nonce := ta.txn.TransactionNonce
		if nonce < 1 {
			nonce = transaction.Cache.GetNextNonce(ta.txn.ClientID)
		} else {
			transaction.Cache.Set(ta.txn.ClientID, nonce)
		}
		ta.txn.TransactionNonce = nonce
		ta.txn.ComputeHashAndSignWithWallet(signWithWallet, w)
		ta.submitTxn()
	}()
	return nil
}

func (ta *TransactionWithAuth) Output() []byte {
	return []byte(ta.txnOut)
}

// GetTransactionNonce returns nonce
func (ta *TransactionWithAuth) GetTransactionNonce() int64 {
	return ta.txn.TransactionNonce
}

// ========================================================================== //
//                                vesting pool                                //
// ========================================================================== //

func (ta *TransactionWithAuth) VestingTrigger(poolID string) (err error) {
	err = ta.vestingPoolTxn(transaction.VESTING_TRIGGER, poolID, 0)
	if err != nil {
		logging.Error(err)
		return
	}
	go func() { ta.submitTxn() }()
	return
}

func (ta *TransactionWithAuth) VestingStop(sr *VestingStopRequest) (err error) {
	err = ta.createSmartContractTxn(VestingSmartContractAddress,
		transaction.VESTING_STOP, sr, 0)
	if err != nil {
		logging.Error(err)
		return
	}
	go func() { ta.submitTxn() }()
	return
}

func (ta *TransactionWithAuth) VestingUnlock(poolID string) (err error) {

	err = ta.vestingPoolTxn(transaction.VESTING_UNLOCK, poolID, 0)
	if err != nil {
		logging.Error(err)
		return
	}
	go func() { ta.submitTxn() }()
	return
}

func (ta *TransactionWithAuth) VestingDelete(poolID string) (err error) {
	err = ta.vestingPoolTxn(transaction.VESTING_DELETE, poolID, 0)
	if err != nil {
		logging.Error(err)
		return
	}
	go func() { ta.submitTxn() }()
	return
}

//
// miner sc
//

func (ta *TransactionWithAuth) MinerSCUnlock(nodeID string) (
	err error) {

	mscul := MinerSCUnlock{
		ID: nodeID,
	}

	err = ta.createSmartContractTxn(MinerSmartContractAddress,
		transaction.MINERSC_UNLOCK, &mscul, 0)
	if err != nil {
		logging.Error(err)
		return
	}
	go func() { ta.submitTxn() }()
	return
}

//RegisterMultiSig register a multisig wallet with the SC.
func (ta *TransactionWithAuth) RegisterMultiSig(walletstr string, mswallet string) error {
	return errors.New("", "not implemented")
}

//
// Storage SC
//
