package orderservice

import (
	"bytes"
	"encoding/json"
	"net/http"
	// "strings"
	"testing"
	"math/rand"
	"crypto/md5"
	"encoding/hex"

	"github.com/Jeffail/gabs"

	"github.com/camlabs/camorder"

	// "github.com/dghubble/sling"

	// "github.com/camlabs/camorder/model"
	// "github.com/stretchr/testify/assert"
)

// func TestCreateWallet(t *testing.T) {
// 	resp, err := http.Post("http://localhost:8000/wallet/xxxxx/test", "application/json", strings.NewReader("{}"))

// 	if assert.NoError(t, err) {
// 		assert.Equal(t, 200, resp.StatusCode)
// 	}

// }

// func TestDeleteWallet(t *testing.T) {

// 	req, err := http.NewRequest(http.MethodDelete, "http://localhost:8000/wallet/xxxxx/test", nil)

// 	assert.NoError(t, err)

// 	resp, err := http.DefaultClient.Do(req)

// 	if assert.NoError(t, err) {
// 		assert.Equal(t, 200, resp.StatusCode)
// 	}
// }

// func TestCreateOrder(t *testing.T) {

// 	order, err := json.Marshal(&model.Order{
// 		Tx:    "xxxxxx",
// 		From:  "xxxxxxx",
// 		To:    "xxxxxxx",
// 		Asset: "xxxxxxxxxxx",
// 		Value: "1",
// 	})

// 	assert.NoError(t, err)

// 	resp, err := http.Post("http://localhost:8000/order", "application/json", bytes.NewReader(order))

// 	if assert.NoError(t, err) {
// 		assert.Equal(t, 200, resp.StatusCode)
// 	}
// }

// func TestConfirmOrder(t *testing.T) {

// 	resp, err := http.Post("http://localhost:8000/order/xxxxxx", "application/json", bytes.NewReader([]byte{}))

// 	if assert.NoError(t, err) {
// 		assert.Equal(t, 200, resp.StatusCode)
// 	}
// }

// func TestListOrder(t *testing.T) {
// 	request, err := sling.New().Get("http://localhost:8000/orders/AMpupnF6QweQXLfCtF4dR45FDdKbTXkLsr/0xc56f33fc6ecfcd0c225c4ab356fee59390af8560be0e930faebe74a6daff7c9b/0/10").Request()

// 	assert.NoError(t, err)

// 	var orders []*model.Order
// 	var errmsg interface{}

// 	_, err = sling.New().Do(request, &orders, &errmsg)
// 	assert.NoError(t, err)

// 	assert.NotZero(t, len(orders))

// }

func TestPushMsg(t *testing.T){

	token := "wzU1ZsHnFBiIzPocLWpRiEv6HMtlPRj8xJDTyF6T1XrZwtEoEXIbfxCh8jyFa0GQtQ8tdPFx26NhIcMolj3tW2KEf02fedJo94la"

	var pushMsg *orderservice.PushTxMessage = &orderservice.PushTxMessage{}

	pushMsg.Action = "nodePushMessage"
	pushMsg.AppId = "1536040633"
	pushMsg.Format = "json"
	pushMsg.Method = "avoid"
	pushMsg.Module = "Tool"
	pushMsg.SignMethod = "md5"
	pushMsg.Nonce = string(rand.Intn(100000000))
	pushMsg.TransactionID = "0x4584e64f0b63d7b103904eb8bd2083e3bcda8e1377a1202e8843c83e00a77abc"
	pushMsg.WalletTypeName = "cam"
	pushMsg.Handle = string(rand.Intn(100000000))
	pushMsg.Sign = md5Convert(pushMsg.Nonce + token + pushMsg.Handle)

	webaddr := "http://192.168.10.152/api/index"

	body,err := json.Marshal(pushMsg)

	if err != nil{
		return
	}

	if resp, err := http.Post(webaddr,
		"application/json",
		bytes.NewBuffer(body)); err == nil {
		jsonParsed, _ := gabs.ParseJSONBuffer(resp.Body)
		
		println(pushMsg.TransactionID+"推送成功:"+jsonParsed.String())

	} else {
		println(pushMsg.TransactionID+"推送失败:"+err.Error())
	}
}

func md5Convert(msg string) string{
	h := md5.New()
	h.Write([]byte(msg))
	cipherStr := h.Sum(nil)

	reStr := hex.EncodeToString(cipherStr)

	return reStr
}
