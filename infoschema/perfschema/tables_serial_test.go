// Copyright 2021 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package perfschema_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"testing"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/parser/terror"
	"github.com/pingcap/tidb/testkit"
	"github.com/stretchr/testify/require"
)

func TestTiKVProfileCPU(t *testing.T) {
	store, clean := newMockStore(t)
	defer clean()

	router := http.NewServeMux()
	mockServer := httptest.NewServer(router)
	mockAddr := strings.TrimPrefix(mockServer.URL, "http://")
	defer mockServer.Close()

	// mock tikv profile
	copyHandler := func(filename string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			file, err := os.Open(filepath.Join(currentSourceDir(), filename))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			defer func() { terror.Log(file.Close()) }()
			_, err = io.Copy(w, file)
			terror.Log(err)
		}
	}
	router.HandleFunc("/debug/pprof/profile", copyHandler("testdata/tikv.cpu.profile"))

	// failpoint setting
	servers := []string{
		strings.Join([]string{"tikv", mockAddr, mockAddr}, ","),
		strings.Join([]string{"pd", mockAddr, mockAddr}, ","),
	}
	fpExpr := strings.Join(servers, ";")
	fpName := "github.com/pingcap/tidb/infoschema/perfschema/mockRemoteNodeStatusAddress"
	require.NoError(t, failpoint.Enable(fpName, fmt.Sprintf(`return("%s")`, fpExpr)))
	defer func() { require.NoError(t, failpoint.Disable(fpName)) }()

	tk := testkit.NewTestKit(t, store)

	tk.MustExec("use performance_schema")
	result := tk.MustQuery("select function, percent_abs, percent_rel from tikv_profile_cpu where depth < 3")

	warnings := tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	result.Check(testkit.Rows(
		"root 100% 100%",
		"├─tikv::server::load_statistics::linux::ThreadLoadStatistics::record::h59facb8d680e7794 75.00% 75.00%",
		"│ └─procinfo::pid::stat::stat_task::h69e1aa2c331aebb6 75.00% 100%",
		"├─nom::nom::digit::h905aaaeff7d8ec8e 16.07% 16.07%",
		"│ ├─<core::iter::adapters::Enumerate<I> as core::iter::traits::iterator::Iterator>::next::h16936f9061bb75e4 6.25% 38.89%",
		"│ ├─Unknown 3.57% 22.22%",
		"│ ├─<&u8 as nom::traits::AsChar>::is_dec_digit::he9eacc3fad26ab81 2.68% 16.67%",
		"│ ├─<&[u8] as nom::traits::InputIter>::iter_indices::h6192338433683bff 1.79% 11.11%",
		"│ └─<&[T] as nom::traits::Slice<core::ops::range::RangeFrom<usize>>>::slice::h38d31f11f84aa302 1.79% 11.11%",
		"├─<jemallocator::Jemalloc as core::alloc::GlobalAlloc>::realloc::h5199c50710ab6f9d 1.79% 1.79%",
		"│ └─rallocx 1.79% 100%",
		"├─<jemallocator::Jemalloc as core::alloc::GlobalAlloc>::dealloc::hea83459aa98dd2dc 1.79% 1.79%",
		"│ └─sdallocx 1.79% 100%",
		"├─<jemallocator::Jemalloc as core::alloc::GlobalAlloc>::alloc::hc7962e02169a5c56 0.89% 0.89%",
		"│ └─mallocx 0.89% 100%",
		"├─engine::rocks::util::engine_metrics::flush_engine_iostall_properties::h64a7661c95aa1db7 0.89% 0.89%",
		"│ └─rocksdb::rocksdb::DB::get_map_property_cf::h9722f9040411af44 0.89% 100%",
		"├─core::ptr::real_drop_in_place::h8def0d99e7136f33 0.89% 0.89%",
		"│ └─<alloc::raw_vec::RawVec<T,A> as core::ops::drop::Drop>::drop::h9b59b303bffde02c 0.89% 100%",
		"├─tikv_util::metrics::threads_linux::ThreadInfoStatistics::record::ha8cc290b3f46af88 0.89% 0.89%",
		"│ └─procinfo::pid::stat::stat_task::h69e1aa2c331aebb6 0.89% 100%",
		"├─crossbeam_utils::backoff::Backoff::snooze::h5c121ef4ce616a3c 0.89% 0.89%",
		"│ └─core::iter::range::<impl core::iter::traits::iterator::Iterator for core::ops::range::Range<A>>::next::hdb23ceb766e7a91f 0.89% 100%",
		"└─<hashbrown::raw::bitmask::BitMaskIter as core::iter::traits::iterator::Iterator>::next::he129c78b3deb639d 0.89% 0.89%",
		"  └─Unknown 0.89% 100%"))

	// We can use current processe profile to mock profile of PD because the PD has the
	// same way of retrieving profile with TiDB. And the purpose of this test case is used
	// to make sure all profile HTTP API have been accessed.
	accessed := map[string]struct{}{}
	handlerFactory := func(name string, debug ...int) func(w http.ResponseWriter, _ *http.Request) {
		debugLevel := 0
		if len(debug) > 0 {
			debugLevel = debug[0]
		}
		return func(w http.ResponseWriter, _ *http.Request) {
			profile := pprof.Lookup(name)
			if profile == nil {
				http.Error(w, fmt.Sprintf("profile %s not found", name), http.StatusBadRequest)
				return
			}
			if err := profile.WriteTo(w, debugLevel); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			accessed[name] = struct{}{}
		}
	}

	// mock PD profile
	router.HandleFunc("/pd/api/v1/debug/pprof/profile", copyHandler("../../util/profile/testdata/test.pprof"))
	router.HandleFunc("/pd/api/v1/debug/pprof/heap", handlerFactory("heap"))
	router.HandleFunc("/pd/api/v1/debug/pprof/mutex", handlerFactory("mutex"))
	router.HandleFunc("/pd/api/v1/debug/pprof/allocs", handlerFactory("allocs"))
	router.HandleFunc("/pd/api/v1/debug/pprof/block", handlerFactory("block"))
	router.HandleFunc("/pd/api/v1/debug/pprof/goroutine", handlerFactory("goroutine", 2))

	tk.MustQuery("select * from pd_profile_cpu where depth < 3")
	warnings = tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	tk.MustQuery("select * from pd_profile_memory where depth < 3")
	warnings = tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	tk.MustQuery("select * from pd_profile_mutex where depth < 3")
	warnings = tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	tk.MustQuery("select * from pd_profile_allocs where depth < 3")
	warnings = tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	tk.MustQuery("select * from pd_profile_block where depth < 3")
	warnings = tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	tk.MustQuery("select * from pd_profile_goroutines")
	warnings = tk.Session().GetSessionVars().StmtCtx.GetWarnings()
	require.Lenf(t, warnings, 0, "expect no warnings, but found: %+v", warnings)

	require.Lenf(t, accessed, 5, "expect all HTTP API had been accessed, but found: %v", accessed)
}
