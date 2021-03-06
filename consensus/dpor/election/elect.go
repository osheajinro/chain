// Copyright 2018 The cpchain authors
// This file is part of the cpchain library.
//
// The cpchain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The cpchain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the cpchain library. If not, see <http://www.gnu.org/licenses/>.

// Package election implements dpor's election method.
package election

import (
	"math"
	"math/rand"
	"sort"

	"bitbucket.org/cpchain/chain/commons/log"
	"bitbucket.org/cpchain/chain/consensus/dpor/rpt"
	"github.com/ethereum/go-ethereum/common"
)

// randRange returns a random integer between [ min(i,j), max(i,j) )
// NB, the rhs is open.
func randRange(i, j float64, myrand *rand.Rand) float64 {
	if j > i {
		return i + myrand.Float64()*(j-i)
	}
	return j + myrand.Float64()*(i-j)
}

func findNearest(array []int64, target float64) (int64, int) {
	if len(array) == 0 {
		panic("array length must be nonzero.")
	}

	lo, hi := 0, len(array)-1

	// invariant: the nearest number is always within [lo,hi]
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		if float64(array[mid]) >= target {
			hi = mid
		} else {
			lo = mid
		}
	}

	if float64(array[lo]) >= target {
		return array[lo], lo
	} else if float64(array[hi]) <= target {
		return array[hi], hi
	} else {
		// ok.  we check the gap
		if target-float64(array[lo]) <= float64(array[hi])-target {
			return array[lo], lo
		}
		return array[hi], hi
	}
}

// Elect returns election result of the given rpt list of candidates,
// seed and termLength.
func Elect(rpts rpt.RptList, seed int64, termLen int) []common.Address {
	sort.Sort(rpts)
	sortedRpts := rpts

	log.Debug("length of rpts", "len", len(rpts))
	for _, r := range rpts {
		log.Debug("rptlist", "addr", r.Address.Hex(), "rpt", r.Rpt)
	}

	randSource := rand.NewSource(seed)
	myRand := rand.New(randSource)

	upper := 10.0
	lower := 0.0
	step := (upper - lower) / float64(termLen)

	var randoms []float64

	for i := 0; i < termLen; i++ {
		rnd := randRange(float64(i)*step, float64(i+1)*step, myRand)
		randoms = append(randoms, math.Log2(float64(1.0+rnd)))
	}

	scale := sortedRpts[len(sortedRpts)-1].Rpt / int64(math.Log2(float64(1+upper)))
	scaledRpts := sortedRpts
	for i := 0; i < len(sortedRpts); i++ {
		scaledRpts[i].Rpt /= scale
	}

	elected := make([]common.Address, termLen)

	for i := 0; i < termLen; i++ {
		var srpts []int64 //
		for j := 0; j < len(scaledRpts); j++ {
			srpts = append(srpts, scaledRpts[j].Rpt)
		}
		_, pos := findNearest(srpts, randoms[i])

		elected[i] = scaledRpts[pos].Address
		scaledRpts = append(scaledRpts[:pos], scaledRpts[pos+1:]...)

	}
	return elected
}

// Elect2 simplifies the election method
//
// In election, a certain number of candidates (referred as *seats*) are elected to be proposer
// according to their RPT value.
// We have two principles to design the election:
//
// An RNode with higher RPT has higher chance to be elected;
// Each term of proposers has a certain number of representatives from RNodes with low RPT.
//
// Thus, the main ideas of election process are:
//
// Candidates are divided into two partitions, high-RPT RNodes and low-RPT RNodes;
// Either partition has a number of available seats;
// The probability mass for each node being elected is proportional to its RPT in its corresponding partition.
//
// rpts: the reputation list of RNodes
// seed: a seed to generate a series of random numbers to select RNodes
// totalSeats: total seats of the election result
// lowRptCount: the number of low Rpt RNodes among the total RNodes
// lowRptSeats: the number of seats for low Rpt RNodes in the Proposer Committee
func Elect2(rpts rpt.RptList, seed int64, totalSeats int, lowRptCount int, lowRptSeats int) []common.Address {
	if lowRptCount > rpts.Len() || lowRptSeats > totalSeats || totalSeats > rpts.Len() || lowRptCount < lowRptSeats {
		return []common.Address{}
	}

	sort.Sort(rpts)

	lowRpts := rpts[:lowRptCount]
	highRpts := rpts[lowRptCount:]

	randSource := rand.NewSource(seed)
	myRand := rand.New(randSource)

	lowElected := randomSelectByRpt(lowRpts, myRand, lowRptSeats)
	highElected := randomSelectByRpt(highRpts, myRand, totalSeats-lowRptSeats)

	return append(lowElected, highElected...)
}

// randomSelectByRpt
// uniform random selection from rptPartition
// the mass probability for each node being elected is proportional to its RPT
// the function select l random addresses
// and return them as result
func randomSelectByRpt(rpts rpt.RptList, myRand *rand.Rand, seats int) (result []common.Address) {
	// each element in rptPartition is referred as rpt
	// then we sum all rpt values, as sumRpt
	// random select l addresses according to its rpt/sumRpt
	// return these l addresses
	sort.Sort(rpts)

	sums, sum := sumOfFirstN(rpts)
	selected := make(map[int]struct{})

	for seats > 0 {
		randI := myRand.Int63n(sum)
		resultIdx := findHit(randI, sums)

		// if already selected, continue
		if _, already := selected[resultIdx]; already {
			continue
		}

		// not selected yet, append it!
		selected[resultIdx] = struct{}{}
		result = append(result, rpts[resultIdx].Address)

		seats--
	}
	return result
}

func findHit(hit int64, hitSums []int64) int {
	for idx, x := range hitSums {
		if hit <= x {
			return idx
		}
	}
	return len(hitSums) - 1
}

func sumOfFirstN(rpts rpt.RptList) (sums []int64, sum int64) {
	sum = 0
	for _, rpt := range rpts {
		sum += rpt.Rpt
		sums = append(sums, sum)
	}
	return
}
