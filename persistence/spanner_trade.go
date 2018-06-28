package persistence

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"time"

	"cloud.google.com/go/spanner"
	"github.com/MixinMessenger/go-number"
	"github.com/MixinMessenger/ocean.one/engine"
	"github.com/satori/go.uuid"
	"google.golang.org/api/iterator"
)

func (persist *Spanner) ListPendingTransfers(ctx context.Context, limit int) ([]*Transfer, error) {
	txn := persist.spanner.ReadOnlyTransaction()
	defer txn.Close()

	it := txn.Query(ctx, spanner.Statement{
		SQL: fmt.Sprintf("SELECT transfer_id FROM transfers@{FORCE_INDEX=transfers_by_created} ORDER BY created_at LIMIT %d", limit),
	})
	defer it.Stop()

	transferIds := make([]string, 0)
	for {
		row, err := it.Next()
		if err == iterator.Done {
			break
		} else if err != nil {
			return nil, err
		}
		var id string
		err = row.Columns(&id)
		if err != nil {
			return nil, err
		}
		transferIds = append(transferIds, id)
	}

	tit := txn.Query(ctx, spanner.Statement{
		SQL:    "SELECT * FROM transfers WHERE transfer_id IN UNNEST(@transfer_ids)",
		Params: map[string]interface{}{"transfer_ids": transferIds},
	})
	defer tit.Stop()

	transfers := make([]*Transfer, 0)
	for {
		row, err := tit.Next()
		if err == iterator.Done {
			return transfers, nil
		} else if err != nil {
			return transfers, err
		}
		var transfer Transfer
		err = row.ToStruct(&transfer)
		if err != nil {
			return transfers, err
		}
		transfers = append(transfers, &transfer)
	}
}

func (persist *Spanner) ExpireTransfers(ctx context.Context, transfers []*Transfer) error {
	var set []spanner.KeySet
	for _, t := range transfers {
		set = append(set, spanner.Key{t.TransferId})
	}
	_, err := persist.spanner.Apply(ctx, []*spanner.Mutation{
		spanner.Delete("transfers", spanner.KeySets(set...)),
	})
	return err
}

func (persist *Spanner) Transact(ctx context.Context, taker, maker *engine.Order, amount number.Decimal, precision int32) error {
	askTrade, bidTrade := makeTrades(taker, maker, amount, precision)
	askTransfer, bidTransfer := handleFees(askTrade, bidTrade)

	askTradeMutation, err := spanner.InsertStruct("trades", askTrade)
	if err != nil {
		return err
	}
	bidTradeMutation, err := spanner.InsertStruct("trades", bidTrade)
	if err != nil {
		return err
	}

	askTransferMutation, err := spanner.InsertStruct("transfers", askTransfer)
	if err != nil {
		return err
	}
	bidTransferMutation, err := spanner.InsertStruct("transfers", bidTransfer)
	if err != nil {
		return err
	}

	mutations := makeOrderMutations(taker, maker, amount, precision)
	mutations = append(mutations, askTradeMutation, bidTradeMutation)
	mutations = append(mutations, askTransferMutation, bidTransferMutation)
	_, err = persist.spanner.Apply(ctx, mutations)
	return err
}

func (persist *Spanner) CancelOrder(ctx context.Context, order *engine.Order, precision int32) error {
	filledPrice := number.FromString(fmt.Sprint(order.FilledPrice)).Mul(number.New(1, -precision)).Persist()
	orderCols := []string{"order_id", "filled_amount", "remaining_amount", "filled_price", "state"}
	orderVals := []interface{}{order.Id, order.FilledAmount.Persist(), order.RemainingAmount.Persist(), filledPrice, OrderStateDone}
	mutations := []*spanner.Mutation{
		spanner.Update("orders", orderCols, orderVals),
		spanner.Delete("actions", spanner.Key{order.Id, engine.OrderActionCreate}),
		spanner.Delete("actions", spanner.Key{order.Id, engine.OrderActionCancel}),
	}

	transfer := &Transfer{
		TransferId: getSettlementId(order.Id, engine.OrderActionCancel),
		Source:     TransferSourceOrder,
		Detail:     order.Id,
		AssetId:    order.Quote,
		Amount:     order.RemainingAmount.Persist(),
		CreatedAt:  time.Now(),
		UserId:     order.UserId,
	}
	if order.Side == engine.PageSideAsk {
		transfer.AssetId = order.Base
	}
	transferMutation, err := spanner.InsertStruct("transfers", transfer)
	if err != nil {
		return err
	}
	mutations = append(mutations, transferMutation)
	_, err = persist.spanner.Apply(ctx, mutations)
	return err
}

func (persist *Spanner) ReadTransferTrade(ctx context.Context, tradeId, assetId string) (*Trade, error) {
	it := persist.spanner.Single().Query(ctx, spanner.Statement{
		SQL:    "SELECT * FROM trades WHERE trade_id=@trade_id",
		Params: map[string]interface{}{"trade_id": tradeId},
	})
	defer it.Stop()

	for {
		row, err := it.Next()
		if err == iterator.Done {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		var trade Trade
		err = row.ToStruct(&trade)
		if err != nil {
			return nil, err
		}
		if trade.FeeAssetId == assetId {
			return &trade, nil
		}
	}
}

func makeOrderMutations(taker, maker *engine.Order, amount number.Decimal, precision int32) []*spanner.Mutation {
	makerFilledPrice := number.FromString(fmt.Sprint(maker.FilledPrice)).Mul(number.New(1, -precision)).Persist()
	takerFilledPrice := number.FromString(fmt.Sprint(taker.FilledPrice)).Mul(number.New(1, -precision)).Persist()

	takerOrderCols := []string{"order_id", "filled_amount", "remaining_amount", "filled_price"}
	takerOrderVals := []interface{}{taker.Id, taker.FilledAmount.Persist(), taker.RemainingAmount.Persist(), takerFilledPrice}
	makerOrderCols := []string{"order_id", "filled_amount", "remaining_amount", "filled_price"}
	makerOrderVals := []interface{}{maker.Id, maker.FilledAmount.Persist(), maker.RemainingAmount.Persist(), makerFilledPrice}
	if taker.RemainingAmount.Sign() == 0 {
		takerOrderCols = append(takerOrderCols, "state")
		takerOrderVals = append(takerOrderVals, OrderStateDone)
	}
	if maker.RemainingAmount.Sign() == 0 {
		makerOrderCols = append(makerOrderCols, "state")
		makerOrderVals = append(makerOrderVals, OrderStateDone)
	}
	mutations := []*spanner.Mutation{
		spanner.Update("orders", takerOrderCols, takerOrderVals),
		spanner.Update("orders", makerOrderCols, makerOrderVals),
	}

	if taker.RemainingAmount.Sign() == 0 {
		mutations = append(mutations, spanner.Delete("actions", spanner.Key{taker.Id, engine.OrderActionCreate}))
		mutations = append(mutations, spanner.Delete("actions", spanner.Key{taker.Id, engine.OrderActionCancel}))
	}
	if maker.RemainingAmount.Sign() == 0 {
		mutations = append(mutations, spanner.Delete("actions", spanner.Key{maker.Id, engine.OrderActionCreate}))
		mutations = append(mutations, spanner.Delete("actions", spanner.Key{maker.Id, engine.OrderActionCancel}))
	}
	return mutations
}

func makeTrades(taker, maker *engine.Order, amount number.Decimal, precision int32) (*Trade, *Trade) {
	tradeId, _ := uuid.NewV4()
	askOrderId, bidOrderId := taker.Id, maker.Id
	if taker.Side == engine.PageSideBid {
		askOrderId, bidOrderId = maker.Id, taker.Id
	}
	price := number.FromString(fmt.Sprint(maker.Price)).Mul(number.New(1, -precision))

	takerTrade := &Trade{
		TradeId:      tradeId.String(),
		Liquidity:    TradeLiquidityTaker,
		AskOrderId:   askOrderId,
		BidOrderId:   bidOrderId,
		QuoteAssetId: taker.Quote,
		BaseAssetId:  taker.Base,
		Side:         taker.Side,
		Price:        price.Persist(),
		Amount:       amount.Persist(),
		CreatedAt:    time.Now(),
		UserId:       taker.UserId,
	}
	makerTrade := &Trade{
		TradeId:      tradeId.String(),
		Liquidity:    TradeLiquidityMaker,
		AskOrderId:   askOrderId,
		BidOrderId:   bidOrderId,
		QuoteAssetId: maker.Quote,
		BaseAssetId:  maker.Base,
		Side:         maker.Side,
		Price:        price.Persist(),
		Amount:       amount.Persist(),
		CreatedAt:    time.Now(),
		UserId:       maker.UserId,
	}

	askTrade, bidTrade := takerTrade, makerTrade
	if askTrade.Side == engine.PageSideBid {
		askTrade, bidTrade = makerTrade, takerTrade
	}
	return askTrade, bidTrade
}

func handleFees(ask, bid *Trade) (*Transfer, *Transfer) {
	total := number.FromString(ask.Amount).Mul(number.FromString(ask.Price))
	askFee := total.Mul(number.FromString(TakerFeeRate))
	bidFee := number.FromString(bid.Amount).Mul(number.FromString(MakerFeeRate))
	if ask.Liquidity == TradeLiquidityMaker {
		askFee = total.Mul(number.FromString(MakerFeeRate))
		bidFee = number.FromString(bid.Amount).Mul(number.FromString(TakerFeeRate))
	}

	ask.FeeAssetId = ask.QuoteAssetId
	ask.FeeAmount = askFee.Persist()
	bid.FeeAssetId = bid.BaseAssetId
	bid.FeeAmount = bidFee.Persist()

	askTransfer := &Transfer{
		TransferId: getSettlementId(ask.TradeId, ask.Liquidity),
		Source:     TransferSourceTrade,
		Detail:     ask.TradeId,
		AssetId:    ask.FeeAssetId,
		Amount:     total.Sub(askFee).Persist(),
		CreatedAt:  time.Now(),
		UserId:     ask.UserId,
	}
	bidTransfer := &Transfer{
		TransferId: getSettlementId(bid.TradeId, bid.Liquidity),
		Source:     TransferSourceTrade,
		Detail:     bid.TradeId,
		AssetId:    bid.FeeAssetId,
		Amount:     number.FromString(bid.Amount).Sub(bidFee).Persist(),
		CreatedAt:  time.Now(),
		UserId:     bid.UserId,
	}
	return askTransfer, bidTransfer
}

func getSettlementId(id, modifier string) string {
	h := md5.New()
	io.WriteString(h, id)
	io.WriteString(h, modifier)
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x30
	sum[8] = (sum[8] & 0x3f) | 0x80
	return uuid.FromBytesOrNil(sum).String()
}
