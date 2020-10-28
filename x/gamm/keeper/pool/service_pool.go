package pool

import (
	"fmt"

	"github.com/c-osmosis/osmosis/x/gamm/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

type LiquidityPoolTransactor interface {
	CreatePool(
		ctx sdk.Context,
		sender sdk.AccAddress,
		swapFee sdk.Dec,
		lpToken types.LPTokenInfo,
		bindTokens []types.BindTokenInfo,
	) (poolId uint64, err error)

	JoinPool(
		ctx sdk.Context,
		sender sdk.AccAddress,
		targetPoolId uint64,
		poolAmountOut sdk.Int,
		maxAmountsIn []types.MaxAmountIn,
	) (err error)

	JoinPoolWithExternAmountIn(
		ctx sdk.Context,
		sender sdk.AccAddress,
		targetPoolId uint64,
		tokenIn string,
		tokenAmountIn sdk.Int,
		minPoolAmountOut sdk.Int,
	) (poolAmount sdk.Int, err error)

	JoinPoolWithPoolAmountOut(
		ctx sdk.Context,
		sender sdk.AccAddress,
		targetPoolId uint64,
		tokenIn string,
		poolAmountOut sdk.Int,
		maxAmountIn sdk.Int,
	) (poolAmount sdk.Int, err error)

	ExitPool(
		ctx sdk.Context,
		sender sdk.AccAddress,
		targetPoolId uint64,
		poolAmountIn sdk.Int,
		minAmountsOut []types.MinAmountOut,
	) (err error)

	ExitPoolWithPoolAmountIn(
		ctx sdk.Context,
		sender sdk.AccAddress,
		targetPoolId uint64,
		tokenOut string,
		poolAmountIn sdk.Int,
		minAmountOut sdk.Int,
	) (tokenAmount sdk.Int, err error)

	ExitPoolWithExternAmountOut(
		ctx sdk.Context,
		sender sdk.AccAddress,
		targetPoolId uint64,
		tokenOut string,
		tokenAmountOut sdk.Int,
		maxPoolAmountIn sdk.Int,
	) (tokenAmount sdk.Int, err error)
}

var _ LiquidityPoolTransactor = poolService{}

func (p poolService) joinPool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	pool types.Pool,
	swapTargets sdk.Coins,
	swapAmount sdk.Int,
) error {
	// process token transfers
	poolShare := lpService{
		denom:      pool.Token.Denom,
		bankKeeper: p.bankKeeper,
	}
	if err := poolShare.mintPoolShare(ctx, swapAmount); err != nil {
		return err
	}
	if err := poolShare.pushPoolShare(ctx, sender, swapAmount); err != nil {
		return err
	}
	if err := p.bankKeeper.SendCoinsFromAccountToModule(
		ctx,
		sender,
		types.ModuleName,
		swapTargets,
	); err != nil {
		return err
	}

	// save changes
	pool.Token.TotalSupply = pool.Token.TotalSupply.Add(swapAmount)
	for _, target := range swapTargets {
		record := pool.Records[target.Denom]
		record.Balance = record.Balance.Add(target.Amount)
		pool.Records[target.Denom] = record
	}
	p.store.StorePool(ctx, pool)
	return nil
}

func (p poolService) CreatePool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	swapFee sdk.Dec,
	lpToken types.LPTokenInfo,
	bindTokens []types.BindTokenInfo,
) (uint64, error) {
	if len(bindTokens) < 2 {
		return 0, sdkerrors.Wrapf(
			types.ErrInvalidRequest,
			"token info length should be at least 2",
		)
	}
	if len(bindTokens) > 8 {
		return 0, sdkerrors.Wrapf(
			types.ErrInvalidRequest,
			"token info length should be at maximum 8",
		)
	}

	records := make(map[string]types.Record, len(bindTokens))
	for _, info := range bindTokens {
		records[info.Denom] = types.Record{
			DenormalizedWeight: info.Weight,
			Balance:            info.Amount,
		}
	}

	poolId := p.store.GetNextPoolNumber(ctx)
	if lpToken.Denom == "" {
		lpToken.Denom = fmt.Sprintf("osmosis/pool/%d", poolId)
	} else {
		lpToken.Denom = fmt.Sprintf("osmosis/custom/%s", lpToken.Denom)
	}

	totalWeight := sdk.NewDec(0)
	for _, record := range records {
		totalWeight = totalWeight.Add(record.DenormalizedWeight)
	}

	pool := types.Pool{
		Id:      poolId,
		SwapFee: swapFee,
		Token: types.LP{
			Denom:       lpToken.Denom,
			Description: lpToken.Description,
			TotalSupply: sdk.NewInt(0),
		},
		TotalWeight: totalWeight,
		Records:     records,
	}

	p.store.StorePool(ctx, pool)

	var coins sdk.Coins
	for denom, record := range records {
		coins = append(coins, sdk.Coin{
			Denom:  denom,
			Amount: record.Balance,
		})
	}
	if coins == nil {
		panic("oh my god")
	}
	coins = coins.Sort()

	initialSupply := sdk.NewIntWithDecimal(100, 6)
	if err := p.joinPool(ctx, sender, pool, coins, initialSupply); err != nil {
		return 0, err
	}
	return pool.Id, nil
}

func (p poolService) JoinPool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	targetPoolId uint64,
	poolAmountOut sdk.Int,
	maxAmountsIn []types.MaxAmountIn,
) error {
	pool, err := p.store.FetchPool(ctx, targetPoolId)
	if err != nil {
		return err
	}
	lpToken := pool.Token

	poolTotal := lpToken.TotalSupply.ToDec()
	poolRatio := poolAmountOut.ToDec().Quo(poolTotal)
	if poolRatio.Equal(sdk.NewDec(0)) {
		return sdkerrors.Wrapf(types.ErrMathApprox, "calc poolRatio")
	}

	checker := map[string]bool{}
	for _, m := range maxAmountsIn {
		if check := checker[m.Denom]; check {
			return sdkerrors.Wrapf(
				types.ErrInvalidRequest,
				"do not use duplicated denom",
			)
		}
		checker[m.Denom] = true
	}
	if len(pool.Records) != len(checker) {
		return sdkerrors.Wrapf(
			types.ErrInvalidRequest,
			"invalid maxAmountsIn argument",
		)
	}

	var swapTargets sdk.Coins
	for _, maxAmountIn := range maxAmountsIn {
		var (
			tokenDenom    = maxAmountIn.Denom
			record, ok    = pool.Records[tokenDenom]
			tokenAmountIn = poolRatio.Mul(record.Balance.ToDec()).TruncateInt()
		)
		if !ok {
			return sdkerrors.Wrapf(types.ErrInvalidRequest, "token is not bound to pool")
		}
		if tokenAmountIn.Equal(sdk.NewInt(0)) {
			return sdkerrors.Wrapf(types.ErrMathApprox, "calc tokenAmountIn")
		}
		if tokenAmountIn.GT(maxAmountIn.MaxAmount) {
			return sdkerrors.Wrapf(types.ErrLimitExceed, "max amount limited")
		}
		swapTargets = append(swapTargets, sdk.Coin{
			Denom:  tokenDenom,
			Amount: tokenAmountIn,
		})
	}
	return p.joinPool(ctx, sender, pool, swapTargets, poolAmountOut)
}

func (p poolService) JoinPoolWithExternAmountIn(
	ctx sdk.Context,
	sender sdk.AccAddress,
	targetPoolId uint64,
	tokenIn string,
	tokenAmountIn sdk.Int,
	minPoolAmountOut sdk.Int,
) (sdk.Int, error) {
	pool, err := p.store.FetchPool(ctx, targetPoolId)
	if err != nil {
		return sdk.Int{}, err
	}

	record, ok := pool.Records[tokenIn]
	if !ok {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrNotBound,
			"token %s is not bound to this pool", tokenIn,
		)
	}
	if tokenAmountIn.ToDec().GT(record.Balance.ToDec().Mul(maxInRatio)) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrMaxInRatio,
			"tokenAmount exceeds max in ratio",
		)
	}

	poolAmountOut := calcPoolOutGivenSingleIn(
		record.Balance.ToDec(),
		record.DenormalizedWeight,
		pool.Token.TotalSupply.ToDec(),
		pool.TotalWeight,
		tokenAmountIn.ToDec(),
		pool.SwapFee,
	).TruncateInt()

	if poolAmountOut.LT(minPoolAmountOut) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrLimitOut,
			"poolShare minimum limit has exceeded",
		)
	}

	if err := p.joinPool(
		ctx,
		sender,
		pool,
		sdk.Coins{{tokenIn, tokenAmountIn}},
		poolAmountOut,
	); err != nil {
		return sdk.Int{}, err
	}

	return poolAmountOut, nil
}

func (p poolService) JoinPoolWithPoolAmountOut(
	ctx sdk.Context,
	sender sdk.AccAddress,
	targetPoolId uint64,
	tokenIn string,
	poolAmountOut sdk.Int,
	maxAmountIn sdk.Int,
) (sdk.Int, error) {
	pool, err := p.store.FetchPool(ctx, targetPoolId)
	if err != nil {
		return sdk.Int{}, err
	}

	record, ok := pool.Records[tokenIn]
	if !ok {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrNotBound,
			"token %s is not bound to this pool", tokenIn,
		)
	}

	tokenAmountIn := calcSingleInGivenPoolOut(
		record.Balance.ToDec(),
		record.DenormalizedWeight,
		pool.Token.TotalSupply.ToDec(),
		pool.TotalWeight,
		poolAmountOut.ToDec(),
		pool.SwapFee,
	).TruncateInt()
	if tokenAmountIn.Equal(sdk.NewInt(0)) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrMathApprox,
			"calculate tokenAmountIn",
		)
	}
	if tokenAmountIn.GT(maxAmountIn) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrLimitIn,
			"tokenAmount maximum limit has exceeded",
		)
	}

	if tokenAmountIn.ToDec().GT(record.Balance.ToDec().Mul(maxInRatio)) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrMaxInRatio,
			"tokenAmount exceeds max in ratio",
		)
	}

	if err := p.joinPool(
		ctx,
		sender,
		pool,
		sdk.Coins{{tokenIn, tokenAmountIn}},
		poolAmountOut,
	); err != nil {
		return sdk.Int{}, err
	}

	return poolAmountOut, nil
}

func (p poolService) exitPool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	pool types.Pool,
	swapTarget sdk.Int,
	swapAmounts sdk.Coins,
) error {
	poolShare := lpService{
		denom:      pool.Token.Denom,
		bankKeeper: p.bankKeeper,
	}
	if err := poolShare.pullPoolShare(ctx, sender, swapTarget); err != nil {
		return err
	}
	if err := poolShare.burnPoolShare(ctx, swapTarget); err != nil {
		return err
	}
	err := p.bankKeeper.SendCoinsFromModuleToAccount(
		ctx,
		types.ModuleName,
		sender,
		swapAmounts,
	)
	if err != nil {
		return err
	}

	// save changes
	pool.Token.TotalSupply = pool.Token.TotalSupply.Sub(swapTarget)
	for _, target := range swapAmounts {
		record := pool.Records[target.Denom]
		record.Balance = record.Balance.Sub(target.Amount)
		pool.Records[target.Denom] = record
	}
	p.store.StorePool(ctx, pool)
	return nil
}

func (p poolService) ExitPool(
	ctx sdk.Context,
	sender sdk.AccAddress,
	targetPoolId uint64,
	poolAmountIn sdk.Int,
	minAmountsOut []types.MinAmountOut,
) error {
	pool, err := p.store.FetchPool(ctx, targetPoolId)
	if err != nil {
		return err
	}
	lpToken := pool.Token

	poolTotal := lpToken.TotalSupply.ToDec()
	poolRatio := poolAmountIn.ToDec().Quo(poolTotal)
	if poolRatio.Equal(sdk.NewDec(0)) {
		return sdkerrors.Wrapf(types.ErrMathApprox, "calc poolRatio")
	}

	checker := map[string]bool{}
	for _, m := range minAmountsOut {
		if check := checker[m.Denom]; check {
			return sdkerrors.Wrapf(
				types.ErrInvalidRequest,
				"do not use duplicated denom",
			)
		}
		checker[m.Denom] = true
	}
	if len(pool.Records) != len(checker) {
		return sdkerrors.Wrapf(
			types.ErrInvalidRequest,
			"invalid minAmountsOut argument",
		)
	}

	var swapAmounts sdk.Coins
	for _, minAmountOut := range minAmountsOut {
		var (
			tokenDenom     = minAmountOut.Denom
			record, ok     = pool.Records[tokenDenom]
			tokenAmountOut = poolRatio.Mul(record.Balance.ToDec()).TruncateInt()
		)
		if !ok {
			return sdkerrors.Wrapf(types.ErrInvalidRequest, "token is not bound to pool")
		}
		if tokenAmountOut.Equal(sdk.NewInt(0)) {
			return sdkerrors.Wrapf(types.ErrMathApprox, "calc tokenAmountOut")
		}
		if tokenAmountOut.LT(minAmountOut.MinAmount) {
			return sdkerrors.Wrapf(types.ErrLimitExceed, "min amount limited")
		}
		record.Balance = record.Balance.Sub(tokenAmountOut)
		pool.Records[tokenDenom] = record

		swapAmounts = append(swapAmounts, sdk.Coin{
			Denom:  tokenDenom,
			Amount: tokenAmountOut,
		})
	}
	return p.exitPool(ctx, sender, pool, poolAmountIn, swapAmounts)
}

func (p poolService) ExitPoolWithPoolAmountIn(
	ctx sdk.Context,
	sender sdk.AccAddress,
	targetPoolId uint64,
	tokenOut string,
	poolAmountIn sdk.Int,
	minAmountOut sdk.Int,
) (sdk.Int, error) {
	pool, err := p.store.FetchPool(ctx, targetPoolId)
	if err != nil {
		return sdk.Int{}, err
	}

	record, ok := pool.Records[tokenOut]
	if !ok {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrNotBound,
			"token %s is not bound to this pool", tokenOut,
		)
	}

	tokenAmountOut := calcSingleOutGivenPoolIn(
		record.Balance.ToDec(),
		record.DenormalizedWeight,
		pool.Token.TotalSupply.ToDec(),
		pool.TotalWeight,
		poolAmountIn.ToDec(),
		pool.SwapFee,
	).TruncateInt()
	if tokenAmountOut.LT(minAmountOut) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrLimitOut,
			"tokenAmount minimum limit has exceeded",
		)
	}
	if tokenAmountOut.ToDec().GT(record.Balance.ToDec().Mul(maxOutRatio)) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrMaxOutRatio,
			"tokenAmount exceeds max out ratio")
	}

	if err := p.exitPool(
		ctx,
		sender,
		pool,
		poolAmountIn,
		sdk.Coins{{tokenOut, tokenAmountOut}},
	); err != nil {
		return sdk.Int{}, err
	}

	return tokenAmountOut, nil
}

func (p poolService) ExitPoolWithExternAmountOut(
	ctx sdk.Context,
	sender sdk.AccAddress,
	targetPoolId uint64,
	tokenOut string,
	tokenAmountOut sdk.Int,
	maxPoolAmountIn sdk.Int,
) (sdk.Int, error) {
	pool, err := p.store.FetchPool(ctx, targetPoolId)
	if err != nil {
		return sdk.Int{}, err
	}

	record, ok := pool.Records[tokenOut]
	if !ok {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrNotBound,
			"token %s is not bound to this pool", tokenOut,
		)
	}
	if tokenAmountOut.ToDec().GT(record.Balance.ToDec().Mul(maxOutRatio)) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrMaxOutRatio,
			"tokenAmount exceeds max out ratio")
	}

	poolAmountIn := calcPoolInGivenSingleOut(
		record.Balance.ToDec(),
		record.DenormalizedWeight,
		pool.Token.TotalSupply.ToDec(),
		pool.TotalWeight,
		tokenAmountOut.ToDec(),
		pool.SwapFee,
	).TruncateInt()
	if poolAmountIn.Equal(sdk.NewInt(0)) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrMathApprox,
			"calculate poolAmountIn",
		)
	}
	if poolAmountIn.GT(maxPoolAmountIn) {
		return sdk.Int{}, sdkerrors.Wrapf(
			types.ErrLimitIn,
			"poolAmount maximum limit has exceeded",
		)
	}

	if err := p.exitPool(
		ctx,
		sender,
		pool,
		poolAmountIn,
		sdk.Coins{{tokenOut, tokenAmountOut}},
	); err != nil {
		return sdk.Int{}, err
	}

	return poolAmountIn, nil
}
