package keeper

import (
	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/codec"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
	"github.com/tendermint/tendermint/libs/log"

	errorsignite "github.com/ignite/modules/pkg/errors"
	"github.com/ignite/modules/x/mint/types"
)

// Keeper of the mint store
type Keeper struct {
	cdc              codec.BinaryCodec
	storeKey         storetypes.StoreKey
	paramSpace       paramtypes.Subspace
	stakingKeeper    types.StakingKeeper
	accountKeeper    types.AccountKeeper
	bankKeeper       types.BankKeeper
	distrKeeper      types.DistrKeeper
	feeCollectorName string

	// the address capable of executing a MsgUpdateParams message. Typically, this
	// should be the x/gov module account.
	authority string
}

// NewKeeper creates a new mint Keeper instance
func NewKeeper(
	cdc codec.BinaryCodec, key storetypes.StoreKey, sk types.StakingKeeper,
	ak types.AccountKeeper, bk types.BankKeeper, dk types.DistrKeeper,
	feeCollectorName string, authority string,
) Keeper {
	// ensure mint module account is set
	if addr := ak.GetModuleAddress(types.ModuleName); addr == nil {
		panic("the mint module account has not been set")
	}

	return Keeper{
		cdc:              cdc,
		storeKey:         key,
		stakingKeeper:    sk,
		accountKeeper:    ak,
		bankKeeper:       bk,
		distrKeeper:      dk,
		feeCollectorName: feeCollectorName,
		authority:        authority,
	}
}

// GetAuthority returns the x/mint module's authority.
func (k Keeper) GetAuthority() string {
	return k.authority
}

// Logger returns a module-specific logger.
func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", "x/"+types.ModuleName)
}

// GetMinter gets the minter
func (k Keeper) GetMinter(ctx sdk.Context) (minter types.Minter) {
	store := ctx.KVStore(k.storeKey)
	b := store.Get(types.MinterKey)
	if b == nil {
		panic("stored minter should not have been nil")
	}

	k.cdc.MustUnmarshal(b, &minter)
	return
}

// SetMinter sets the minter
func (k Keeper) SetMinter(ctx sdk.Context, minter types.Minter) {
	store := ctx.KVStore(k.storeKey)
	b := k.cdc.MustMarshal(&minter)
	store.Set(types.MinterKey, b)
}

// SetParams sets the x/mint module parameters.
func (k Keeper) SetParams(ctx sdk.Context, p types.Params) error {
	if err := p.Validate(); err != nil {
		return err
	}

	store := ctx.KVStore(k.storeKey)
	bz := k.cdc.MustMarshal(&p)
	store.Set(types.ParamsKey, bz)

	return nil
}

// GetParams returns the current x/mint module parameters.
func (k Keeper) GetParams(ctx sdk.Context) (p types.Params) {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get(types.ParamsKey)
	if bz == nil {
		return p
	}

	k.cdc.MustUnmarshal(bz, &p)
	return p
}

// StakingTokenSupply implements an alias call to the underlying staking keeper's
// StakingTokenSupply to be used in BeginBlocker.
func (k Keeper) StakingTokenSupply(ctx sdk.Context) sdkmath.Int {
	return k.stakingKeeper.StakingTokenSupply(ctx)
}

// BondedRatio implements an alias call to the underlying staking keeper's
// BondedRatio to be used in BeginBlocker.
func (k Keeper) BondedRatio(ctx sdk.Context) sdk.Dec {
	return k.stakingKeeper.BondedRatio(ctx)
}

// MintCoin implements an alias call to the underlying supply keeper's
// MintCoin to be used in BeginBlocker.
func (k Keeper) MintCoin(ctx sdk.Context, coin sdk.Coin) error {
	return k.bankKeeper.MintCoins(ctx, types.ModuleName, sdk.NewCoins(coin))
}

// GetProportion gets the balance of the `MintedDenom` from minted coins and returns coins according to the `AllocationRatio`.
func (k Keeper) GetProportion(ctx sdk.Context, mintedCoin sdk.Coin, ratio sdk.Dec) sdk.Coin {
	return sdk.NewCoin(mintedCoin.Denom, sdk.NewDecFromInt(mintedCoin.Amount).Mul(ratio).TruncateInt())
}

// DistributeMintedCoin implements distribution of minted coins from mint
// to be used in BeginBlocker.
func (k Keeper) DistributeMintedCoin(ctx sdk.Context, mintedCoin sdk.Coin) error {
	params := k.GetParams(ctx)
	proportions := params.DistributionProportions

	// allocate staking rewards into fee collector account to be moved to on next begin blocker by staking module
	stakingRewardsCoins := sdk.NewCoins(k.GetProportion(ctx, mintedCoin, proportions.Staking))
	err := k.bankKeeper.SendCoinsFromModuleToModule(ctx, types.ModuleName, k.feeCollectorName, stakingRewardsCoins)
	if err != nil {
		return err
	}

	fundedAddrsCoin := k.GetProportion(ctx, mintedCoin, proportions.FundedAddresses)
	fundedAddrsCoins := sdk.NewCoins(fundedAddrsCoin)
	if len(params.FundedAddresses) == 0 {
		// fund community pool when rewards address is empty
		if err = k.distrKeeper.FundCommunityPool(
			ctx,
			fundedAddrsCoins,
			k.accountKeeper.GetModuleAddress(types.ModuleName),
		); err != nil {
			return err
		}
	} else {
		// allocate developer rewards to developer addresses by weight
		for _, w := range params.FundedAddresses {
			fundedAddrCoins := sdk.NewCoins(k.GetProportion(ctx, fundedAddrsCoin, w.Weight))
			devAddr, err := sdk.AccAddressFromBech32(w.Address)
			if err != nil {
				return errorsignite.Critical(err.Error())
			}
			err = k.bankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, devAddr, fundedAddrCoins)
			if err != nil {
				return err
			}
		}
	}

	// subtract from original provision to ensure no coins left over after the allocations
	communityPoolCoins := sdk.NewCoins(mintedCoin).Sub(stakingRewardsCoins...).Sub(fundedAddrsCoins...)
	err = k.distrKeeper.FundCommunityPool(ctx, communityPoolCoins, k.accountKeeper.GetModuleAddress(types.ModuleName))
	if err != nil {
		return err
	}

	return err
}
