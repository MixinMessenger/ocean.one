package exchange

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/MixinMessenger/go-number"
	"github.com/MixinMessenger/ocean.one/engine"
	"github.com/satori/go.uuid"
	"github.com/ugorji/go/codec"
)

const (
	BitcoinAssetId = "c6d0c728-2624-429b-8e0d-d9d19b6592fa"
	USDTAssetId    = "815b0b1a-2764-3736-8faa-42d694fa620a"
)

type Error struct {
	Status      int    `json:"status"`
	Code        int    `json:"code"`
	Description string `json:"description"`
}

type Snapshot struct {
	SnapshotId string `json:"snapshot_id"`
	Amount     string `json:"amount"`
	Asset      struct {
		AssetId string `json:"asset_id"`
	} `json:"asset"`
	CreatedAt time.Time `json:"created_at"`

	TraceId    string `json:"trace_id"`
	UserId     string `json:"user_id"`
	OpponentId string `json:"opponent_id"`
	Data       string `json:"data"`
}

type OrderAction struct {
	S string    // side
	A uuid.UUID // asset
	P string    // price
	T string    // type
	O uuid.UUID // order
}

func (ex *Exchange) ensureProcessSnapshot(ctx context.Context, s *Snapshot) {
	for {
		err := ex.processSnapshot(ctx, s)
		if err == nil {
			break
		}
		log.Println("ensureProcessSnapshot", err)
		time.Sleep(100 * time.Millisecond)
	}
}

func (ex *Exchange) processSnapshot(ctx context.Context, s *Snapshot) error {
	if s.UserId != ex.mixinClient.ClientId {
		return nil
	}
	if s.OpponentId == "" || s.TraceId == "" {
		return nil
	}
	if number.FromString(s.Amount).Exhausted() {
		return nil
	}

	action := ex.decryptOrderAction(ctx, s.Data)
	if action == nil {
		return ex.refundSnapshot(ctx, s)
	}
	if action.A.String() == s.Asset.AssetId {
		return ex.refundSnapshot(ctx, s)
	}
	if action.O.String() != uuid.Nil.String() {
		return ex.persist.CancelOrderAction(ctx, action.O.String(), s.CreatedAt)
	}

	if action.T != engine.OrderTypeLimit && action.T != engine.OrderTypeMarket {
		return ex.refundSnapshot(ctx, s)
	}

	amount := number.FromString(s.Amount).RoundFloor(EnginePrecision)
	price := number.FromString(action.P).RoundFloor(EnginePrecision)
	if price.Exhausted() {
		return ex.refundSnapshot(ctx, s)
	}
	if price.Mul(amount).Exhausted() {
		return ex.refundSnapshot(ctx, s)
	}

	var quote, base string
	if action.S == engine.PageSideAsk {
		quote, base = action.A.String(), s.Asset.AssetId
	} else if action.S == engine.PageSideBid {
		quote, base = s.Asset.AssetId, action.A.String()
		amount = amount.Div(price)
	} else {
		return ex.refundSnapshot(ctx, s)
	}
	if !ex.validateQuoteBasePair(quote, base) {
		return ex.refundSnapshot(ctx, s)
	}

	return ex.persist.CreateOrderAction(ctx, s.OpponentId, s.TraceId, action.T, action.S, quote, base, amount, price, s.CreatedAt)
}

func (ex *Exchange) validateQuoteBasePair(quote, base string) bool {
	if quote != BitcoinAssetId && quote != USDTAssetId {
		return false
	}
	if quote == BitcoinAssetId && base == USDTAssetId {
		return false
	}
	return true
}

func (ex *Exchange) refundSnapshot(ctx context.Context, s *Snapshot) error {
	amount := number.FromString(s.Amount).Mul(number.FromString("0.999"))
	if amount.Exhausted() {
		return nil
	}
	h := md5.New()
	io.WriteString(h, s.TraceId)
	io.WriteString(h, "REFUND")
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	traceId := uuid.FromBytesOrNil(sum).String()
	return ex.sendTransfer(ctx, s.OpponentId, s.Asset.AssetId, amount, traceId, "INVALID_ORDER#"+s.TraceId)
}

func (ex *Exchange) decryptOrderAction(ctx context.Context, data string) *OrderAction {
	payload, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	var action OrderAction
	decoder := codec.NewDecoderBytes(payload, ex.codec)
	err = decoder.Decode(&action)
	if err != nil {
		return nil
	}
	return &action
}

func (ex *Exchange) requestMixinNetwork(ctx context.Context, checkpoint time.Time, limit int) ([]*Snapshot, error) {
	uri := fmt.Sprintf("/network/snapshots?offset=%s&order=ASC&limit=%d", checkpoint.Format(time.RFC3339Nano), limit)
	body, err := ex.mixinClient.SendRequest(ctx, "GET", uri, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data  []*Snapshot `json:"data"`
		Error string      `json:"error"`
	}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Data, nil
}

func (ex *Exchange) sendTransfer(ctx context.Context, recipientId, assetId string, amount number.Decimal, traceId, memo string) error {
	if amount.Exhausted() {
		return nil
	}

	pin := ex.mixinClient.EncryptPin()
	data, err := json.Marshal(map[string]interface{}{
		"asset_id":    assetId,
		"opponent_id": recipientId,
		"amount":      amount.Persist(),
		"pin":         pin,
		"trace_id":    traceId,
		"memo":        memo,
	})
	if err != nil {
		return err
	}

	body, err := ex.mixinClient.SendRequest(ctx, "POST", "/transfers", data)
	if err != nil {
		return err
	}

	var resp struct {
		Error Error `json:"error"`
	}
	err = json.Unmarshal(body, &resp)
	if err != nil {
		return err
	}
	if resp.Error.Code > 0 {
		return errors.New(resp.Error.Description)
	}
	return nil
}
