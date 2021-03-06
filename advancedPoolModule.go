package connor

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"time"

	"github.com/sonm-io/core/connor/database"
	"github.com/sonm-io/core/connor/watchers"
	"github.com/sonm-io/core/proto"
	"go.uber.org/zap"
)

/*
	This file for SONM only.
*/

//Tracking hashrate with using Connor's blacklist.
// Get data for 1 hour and another time => Detecting deviation.
func (p *PoolModule) AdvancedPoolHashrateTracking(ctx context.Context, reportedPool watchers.PoolWatcher, avgPool watchers.PoolWatcher) error {
	workers, err := p.c.db.GetWorkersFromDB()
	if err != nil {
		p.c.logger.Error("cannot get worker from pool DB", zap.Error(err))
		return err
	}

	for _, w := range workers {
		if w.BadGuy > 5 {
			continue
		}
		if w.Iterations == 0 {
			newIteration := w.Iterations + 1

			if err := p.c.db.UpdateIterationPoolDB(newIteration, w.DealID); err != nil {
				return err
			}
			continue
		}

		p.c.logger.Info("Update iteration worker less than 4")

		newIteration := w.Iterations + 1
		if err := p.c.db.UpdateIterationPoolDB(newIteration, w.DealID); err != nil {
			return err
		}



		dealInfo, err := p.c.DealClient.Status(ctx, sonm.NewBigInt(big.NewInt(0).SetInt64(w.DealID)))
		if err != nil {
			log.Printf("Cannot get deal from market %v\r\n", w.DealID)
			return err
		}

		bidHashrate, err := p.ReturnBidHashrateForDeal(ctx, dealInfo)
		if err != nil {
			return err
		}

		if w.Iterations < numberOfIterationsForH1 {
			if err := p.UpdateRHPoolData(ctx, reportedPool, p.c.cfg.PoolAddress.EthPoolAddr); err != nil {
				return err
			}
			workerReportedHashrate := uint64(w.WorkerReportedHashrate * hashes)
			p.c.logger.Info("reported hashrate",
				zap.Uint64("RH", workerReportedHashrate),
				zap.Int64("worker", w.DealID))

			if workerReportedHashrate == 0 {
				p.c.logger.Info("worker reported hashrate = 0 send to Connor's blacklist",
					zap.Int64("worker id :", w.DealID),
				)
				err := p.SendToConnorBlackList(ctx, dealInfo)
				if err != nil {
					return err
				}
				continue
			}

			//"change low percent": 6.87285223367698 - пример отклонения на пониженный хешрейт
			changeResponseRHWorker := float64(100 - (float64(workerReportedHashrate*100) / float64(bidHashrate)))
			if changeResponseRHWorker <= 0 {
				p.c.logger.Info("change response reported hashrate worker < 0. It's ok.",
					zap.Int64("worker", w.DealID))
				continue
			}

			p.c.logger.Info("worker deviation (reported hashrate data)",
				zap.Int64("iteration", w.Iterations),
				zap.Float64("change low percent", changeResponseRHWorker),
				zap.Uint64("reported worker hashrate", workerReportedHashrate),
				zap.Uint64("deal hashrate", bidHashrate),
			)

			if err := p.AdvancedDetectingDeviation(ctx, changeResponseRHWorker, w, dealInfo); err != nil {
				return err
			}

		} else {
			err := p.UpdateAvgPoolData(ctx, avgPool, p.c.cfg.PoolAddress.EthPoolAddr+"/1")
			if err != nil {
				return err
			}
			workerAvgHashrate := uint64(w.WorkerAvgHashrate * hashes)

			if workerAvgHashrate == 0 {
				p.c.logger.Info("worker average hashrate = 0 send to Connor's blacklist",
					zap.Int64("worker id :", w.DealID),
				)
				if err := p.SendToConnorBlackList(ctx, dealInfo); err != nil {
					return err
				}
				continue
			}

			changeResponseAvgWorker := float64(100 - (float64(workerAvgHashrate*100) / float64(bidHashrate)))
			if changeResponseAvgWorker <= 0 {
				p.c.logger.Info("change response reported hashrate worker < 0. It's ok.",
					zap.Int64("worker", w.DealID))
				continue
			}

			p.c.logger.Info("Pool inf :: worker deviation (average data)",
				zap.Int64("iteration", w.Iterations),
				zap.Float64("change percent", changeResponseAvgWorker),
				zap.Uint64("reported worker hashrate", workerAvgHashrate),
				zap.Uint64("deal hashrate", bidHashrate),
			)

			if err = p.AdvancedDetectingDeviation(ctx, changeResponseAvgWorker, w, dealInfo); err != nil {
				return err
			}
		}

	}
	return nil
}

//Detects the percentage of deviation of the hashrate and save SupplierID (by MasterID) to Connor's blacklist .
func (p *PoolModule) AdvancedDetectingDeviation(ctx context.Context, changePercentDeviationWorker float64, worker *database.PoolDb, dealInfo *sonm.DealInfoReply) error {

	if changePercentDeviationWorker >= 20 {
		p.c.logger.Info("send worker (supplier Id) to Connor's blacklist", zap.Int64("worker", worker.DealID))
		if err := p.DestroyDeal(ctx, dealInfo); err != nil {
			return err
		}
		err := p.c.db.UpdateBadGayStatusInPoolDB(worker.DealID, int64(BanStatusWORKERINPOOL), time.Now())
		if err != nil {
			return err
		}

	} else if changePercentDeviationWorker >= p.c.cfg.Sensitivity.WorkerLimitChangePercent {
		p.c.logger.Info("Send to Connor's blacklist", zap.Int64("worker", worker.DealID))
		if err := p.SendToConnorBlackList(ctx, dealInfo); err != nil {
			return err
		}
	}
	return nil
}

// Send to Connor's blacklist failed worker. If percent of failed workers more than "cleaner" workers => send Master to blacklist and destroy deal.
func (p *PoolModule) SendToConnorBlackList(ctx context.Context, failedDeal *sonm.DealInfoReply) error {
	workerList, err := p.c.MasterClient.WorkersList(ctx, failedDeal.Deal.MasterID)
	if err != nil {
		return err
	}

	for _, wM := range workerList.Workers {
		val, err := p.c.db.GetFailSupplierFromBlacklistDb(wM.SlaveID.Unwrap().Hex())
		if err != nil {
			return err
		}

		if val == wM.SlaveID.Unwrap().Hex() {
			continue
		} else {
			if err := p.c.db.SaveBlacklistIntoDB(&database.BlackListDb{
				MasterID:       wM.MasterID.Unwrap().Hex(),
				FailSupplierId: wM.SlaveID.Unwrap().Hex(),
				BanStatus:      int64(BanStatusBANNED),
				DealId:         failedDeal.Deal.Id.Unwrap().Int64(),
			}); err != nil {
				return err
			}
		}
	}

	amountFailWorkers, err := p.c.db.GetCountFailSupplierFromDb(failedDeal.Deal.MasterID.String())
	if err != nil {
		return err
	}
	if amountFailWorkers == 0 {
		p.c.logger.Info("amount failed workers is 0")
		return fmt.Errorf("no failed workers in Blacklist")
	}

	amountWorkerInList := len(workerList.Workers)
	if amountFailWorkers == 0 {
		p.c.logger.Info("amount worker in list = 0")
		return fmt.Errorf("worker list is empty")
	}

	clearWorkers := int64(amountWorkerInList) - amountFailWorkers

	percentFailWorkers := float64(amountFailWorkers*100) / float64(amountWorkerInList)

	p.c.logger.Info("Check failed workers in master",
		zap.String("worker id", failedDeal.Deal.MasterID.String()),
		zap.String("deal", failedDeal.Deal.Id.String()),
		zap.Float64("percent failed", percentFailWorkers),
		zap.Int64("amount failed", amountFailWorkers),
		zap.Int64("clear workers", clearWorkers),
	)

	if percentFailWorkers > p.c.cfg.Sensitivity.BadWorkersPercent {
		p.c.logger.Info("The deal destroyed due to the excessive number of banned workers in master.",
			zap.String("deal", failedDeal.Deal.Id.Unwrap().String()),
			zap.Float64("percent failed workers", percentFailWorkers),
			zap.String("MasterId", failedDeal.Deal.MasterID.String()),
		)
		if err := p.DestroyDeal(ctx, failedDeal); err != nil {
			return err
		}
		if err := p.c.db.UpdateBanStatusBlackListDB(failedDeal.Deal.MasterID.Unwrap().Hex(), int64(BanStatusMASTERBAN)); err != nil {
			return err
		}
		if err := p.c.db.UpdateBadGayStatusInPoolDB(failedDeal.Deal.Id.Unwrap().Int64(), int64(BanStatusWORKERINPOOL), time.Now()); err != nil {
			return err
		}
	}
	return nil
}
