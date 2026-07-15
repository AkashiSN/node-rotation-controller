// Scenario P (issue #118) tight-race soak — the 72-rotation ledger, generated
// from the run's analyzer output (soak-analyze.py; canonical record:
// test/e2e/eks-automode/VALIDATION.md § Run 2026-07-15). 71 rows are the main
// pool's graceful surge rotations; the last-but-one row is the epilogue's
// deliberate surge-less forceful fallback, whose null surgeWait/total and
// missing surge node ARE the evidence (spec §3.3: no placeholder on that path).

export interface SoakRotation {
  /** NodeClaim name with the `nodepool-soak-` prefix stripped */
  c: string
  /** pool: 'main' = nodepool-soak, 'epi' = nodepool-soak-epi (epilogue) */
  p: 'main' | 'epi'
  /** completion instant, hours since T0 (see T0 below) */
  t: number
  /** deadline margin at completion, minutes: birth + expireAfter − done */
  m: number
  /** seconds from placeholder creation to surge node Ready; null = surge-less */
  sw: number | null
  /** seconds from old-NodeClaim delete to drain complete */
  dr: number
  /** whole-rotation seconds; null on the surge-less epilogue row */
  tot: number | null
  /** NodeClaim creationTimestamp (UTC) */
  birth: string
  /** rotation completion (UTC) */
  done: string
  /** surge target EC2 instance; null on the surge-less epilogue row */
  node: string | null
}

/** Start of the 12h observation window. */
export const T0 = '2026-07-14T14:20:29Z'
/** The pool's fixed expireAfter (never patched during the run). */
export const EXPIRE_AFTER = '2h12m'

export const ROTATIONS: SoakRotation[] = [
  { c: "fhx2b", p: 'main', t: 0.0, m: 70.3, sw: 40, dr: 44, tot: 83, birth: "2026-07-14T13:18:50Z", done: "2026-07-14T14:20:29Z", node: "i-073168dd1b078525f" },
  { c: "xdjh8", p: 'main', t: 0.2, m: 70.4, sw: 40, dr: 39, tot: 79, birth: "2026-07-14T13:30:53Z", done: "2026-07-14T14:32:30Z", node: "i-09dcbe1f5d6d88d7c" },
  { c: "9xfpf", p: 'main', t: 0.401, m: 70.4, sw: 41, dr: 35, tot: 76, birth: "2026-07-14T13:42:57Z", done: "2026-07-14T14:44:31Z", node: "i-0ea97241df5cd15d2" },
  { c: "xl8fg", p: 'main', t: 0.601, m: 70.4, sw: 24, dr: 48, tot: 73, birth: "2026-07-14T13:55:00Z", done: "2026-07-14T14:56:34Z", node: "i-0e2c826c7f0d82e1e" },
  { c: "ksjxd", p: 'main', t: 0.819, m: 69.5, sw: 53, dr: 77, tot: 131, birth: "2026-07-14T14:07:04Z", done: "2026-07-14T15:09:36Z", node: "i-07c3fd68b28570fb0" },
  { c: "s88b7", p: 'main', t: 1.005, m: 70.4, sw: 38, dr: 38, tot: 76, birth: "2026-07-14T14:19:08Z", done: "2026-07-14T15:20:46Z", node: "i-05701db7c4c26e742" },
  { c: "bvpc6", p: 'main', t: 1.199, m: 70.8, sw: 25, dr: 28, tot: 52, birth: "2026-07-14T14:31:13Z", done: "2026-07-14T15:32:27Z", node: "i-06610ad280717ecb4" },
  { c: "4txxs", p: 'main', t: 1.41, m: 70.2, sw: 38, dr: 49, tot: 87, birth: "2026-07-14T14:43:18Z", done: "2026-07-14T15:45:06Z", node: "i-0732ee95c07077219" },
  { c: "7s7dt", p: 'main', t: 1.612, m: 70.2, sw: 27, dr: 59, tot: 86, birth: "2026-07-14T14:55:23Z", done: "2026-07-14T15:57:11Z", node: "i-028415c25f2709a31" },
  { c: "wbsfb", p: 'main', t: 1.811, m: 70.3, sw: 36, dr: 42, tot: 78, birth: "2026-07-14T15:07:27Z", done: "2026-07-14T16:09:07Z", node: "i-030b61343e3e0405d" },
  { c: "m4jzw", p: 'main', t: 2.016, m: 70.1, sw: 37, dr: 55, tot: 92, birth: "2026-07-14T15:19:32Z", done: "2026-07-14T16:21:26Z", node: "i-08665c82484969e3b" },
  { c: "w4r59", p: 'main', t: 2.208, m: 70.7, sw: 37, dr: 22, tot: 59, birth: "2026-07-14T15:31:37Z", done: "2026-07-14T16:32:58Z", node: "i-0ac7d9f8c7de48fd5" },
  { c: "8bhbp", p: 'main', t: 2.416, m: 70.3, sw: 24, dr: 58, tot: 82, birth: "2026-07-14T15:43:42Z", done: "2026-07-14T16:45:26Z", node: "i-096f1d8ecdea85ea1" },
  { c: "vrtvs", p: 'main', t: 2.622, m: 70.0, sw: 23, dr: 74, tot: 97, birth: "2026-07-14T15:55:47Z", done: "2026-07-14T16:57:47Z", node: "i-0c9f26564102a018e" },
  { c: "zwlds", p: 'main', t: 2.822, m: 70.1, sw: 34, dr: 59, tot: 94, birth: "2026-07-14T16:07:52Z", done: "2026-07-14T17:09:47Z", node: "i-01aebd4e635a7a1ef" },
  { c: "sh6s8", p: 'main', t: 3.026, m: 69.9, sw: 24, dr: 80, tot: 104, birth: "2026-07-14T16:19:56Z", done: "2026-07-14T17:22:03Z", node: "i-01b49d56eb91cc36c" },
  { c: "xhbsc", p: 'main', t: 3.224, m: 70.1, sw: 38, dr: 53, tot: 91, birth: "2026-07-14T16:32:01Z", done: "2026-07-14T17:33:55Z", node: "i-054e29bf46d2ddb04" },
  { c: "hghwx", p: 'main', t: 3.423, m: 70.2, sw: 24, dr: 61, tot: 85, birth: "2026-07-14T16:44:06Z", done: "2026-07-14T17:45:52Z", node: "i-02d851cb5b6421813" },
  { c: "r284b", p: 'main', t: 3.623, m: 70.3, sw: 27, dr: 53, tot: 79, birth: "2026-07-14T16:56:11Z", done: "2026-07-14T17:57:52Z", node: "i-026b29c8cdf04f427" },
  { c: "q592n", p: 'main', t: 3.822, m: 70.5, sw: 27, dr: 44, tot: 71, birth: "2026-07-14T17:08:15Z", done: "2026-07-14T18:09:48Z", node: "i-0299e5a36131f1a53" },
  { c: "rwqpb", p: 'main', t: 4.033, m: 69.9, sw: 24, dr: 81, tot: 105, birth: "2026-07-14T17:20:21Z", done: "2026-07-14T18:22:29Z", node: "i-01362e558662da0a8" },
  { c: "6gdtf", p: 'main', t: 4.218, m: 70.8, sw: 24, dr: 24, tot: 48, birth: "2026-07-14T17:32:26Z", done: "2026-07-14T18:33:35Z", node: "i-01256b5baaa58ea6d" },
  { c: "76xn9", p: 'main', t: 4.433, m: 70.0, sw: 38, dr: 57, tot: 95, birth: "2026-07-14T17:44:30Z", done: "2026-07-14T18:46:28Z", node: "i-09cc4576bdb2f9954" },
  { c: "zw4gm", p: 'main', t: 4.631, m: 70.3, sw: 38, dr: 43, tot: 82, birth: "2026-07-14T17:56:35Z", done: "2026-07-14T18:58:19Z", node: "i-0c8662b78afae7195" },
  { c: "8bcgn", p: 'main', t: 4.836, m: 70.0, sw: 26, dr: 71, tot: 97, birth: "2026-07-14T18:08:40Z", done: "2026-07-14T19:10:40Z", node: "i-09d5061451bc138b9" },
  { c: "8hld7", p: 'main', t: 5.029, m: 70.5, sw: 37, dr: 29, tot: 66, birth: "2026-07-14T18:20:45Z", done: "2026-07-14T19:22:13Z", node: "i-0053ad9546c899cf6" },
  { c: "656fc", p: 'main', t: 5.232, m: 70.4, sw: 38, dr: 34, tot: 73, birth: "2026-07-14T18:32:50Z", done: "2026-07-14T19:34:25Z", node: "i-076c2254a9c78ebd7" },
  { c: "d4d5d", p: 'main', t: 5.442, m: 69.9, sw: 24, dr: 81, tot: 105, birth: "2026-07-14T18:44:55Z", done: "2026-07-14T19:47:01Z", node: "i-0727f919a26d699b2" },
  { c: "8nkrs", p: 'main', t: 5.63, m: 70.7, sw: 23, dr: 33, tot: 56, birth: "2026-07-14T18:56:59Z", done: "2026-07-14T19:58:18Z", node: "i-0b563214cfb435736" },
  { c: "pmlpq", p: 'main', t: 5.836, m: 70.4, sw: 37, dr: 33, tot: 70, birth: "2026-07-14T19:09:04Z", done: "2026-07-14T20:10:38Z", node: "i-07e64b99c9d32e18a" },
  { c: "6wvqx", p: 'main', t: 6.034, m: 70.7, sw: 34, dr: 23, tot: 58, birth: "2026-07-14T19:21:09Z", done: "2026-07-14T20:22:30Z", node: "i-04a2ca7bb98062b91" },
  { c: "kmh4m", p: 'main', t: 6.232, m: 70.8, sw: 25, dr: 23, tot: 48, birth: "2026-07-14T19:33:14Z", done: "2026-07-14T20:34:25Z", node: "i-01d64316889e57c5c" },
  { c: "xdtc9", p: 'main', t: 6.441, m: 70.4, sw: 33, dr: 39, tot: 73, birth: "2026-07-14T19:45:20Z", done: "2026-07-14T20:46:56Z", node: "i-026b484c33d3d619c" },
  { c: "jqmll", p: 'main', t: 6.635, m: 70.8, sw: 24, dr: 22, tot: 46, birth: "2026-07-14T19:57:24Z", done: "2026-07-14T20:58:35Z", node: "i-084de2d8a4645fd56" },
  { c: "xsxbk", p: 'main', t: 6.851, m: 69.9, sw: 36, dr: 64, tot: 100, birth: "2026-07-14T20:09:29Z", done: "2026-07-14T21:11:34Z", node: "i-00d71584622976033" },
  { c: "8tp7t", p: 'main', t: 7.059, m: 69.6, sw: 39, dr: 83, tot: 122, birth: "2026-07-14T20:21:35Z", done: "2026-07-14T21:24:00Z", node: "i-0b502fd445ad0c976" },
  { c: "pdcjt", p: 'main', t: 7.247, m: 70.4, sw: 35, dr: 39, tot: 74, birth: "2026-07-14T20:33:40Z", done: "2026-07-14T21:35:18Z", node: "i-06e1f6f3806f1ec9c" },
  { c: "jnrdw", p: 'main', t: 7.448, m: 70.4, sw: 24, dr: 49, tot: 73, birth: "2026-07-14T20:45:45Z", done: "2026-07-14T21:47:21Z", node: "i-0ab92c05b557f1add" },
  { c: "8hbzb", p: 'main', t: 7.685, m: 68.3, sw: 42, dr: 38, tot: 81, birth: "2026-07-14T20:57:50Z", done: "2026-07-14T22:01:34Z", node: "i-0837901c7b5fb0e3b" },
  { c: "7thv5", p: 'main', t: 7.841, m: 71.0, sw: 37, dr: 23, tot: 60, birth: "2026-07-14T21:09:55Z", done: "2026-07-14T22:10:56Z", node: "i-02f570d8c5fd7fcca" },
  { c: "j8xsl", p: 'main', t: 8.04, m: 71.1, sw: 24, dr: 28, tot: 52, birth: "2026-07-14T21:22:00Z", done: "2026-07-14T22:22:54Z", node: "i-003922ea46c3fb04f" },
  { c: "fkwv7", p: 'main', t: 8.253, m: 70.4, sw: 39, dr: 55, tot: 94, birth: "2026-07-14T21:34:06Z", done: "2026-07-14T22:35:41Z", node: "i-084d041ac9a5866a7" },
  { c: "fjkq9", p: 'main', t: 8.445, m: 71.0, sw: 25, dr: 22, tot: 47, birth: "2026-07-14T21:46:11Z", done: "2026-07-14T22:47:10Z", node: "i-00d419f6d8287f8d8" },
  { c: "vsd8c", p: 'main', t: 8.694, m: 70.2, sw: 39, dr: 61, tot: 100, birth: "2026-07-14T22:00:16Z", done: "2026-07-14T23:02:06Z", node: "i-09324bed1c04586d9" },
  { c: "z9dl8", p: 'main', t: 8.849, m: 70.5, sw: 39, dr: 44, tot: 83, birth: "2026-07-14T22:09:58Z", done: "2026-07-14T23:11:27Z", node: "i-0035175e53982ebb5" },
  { c: "p9qdf", p: 'main', t: 9.047, m: 70.8, sw: 30, dr: 38, tot: 68, birth: "2026-07-14T22:22:03Z", done: "2026-07-14T23:23:18Z", node: "i-05532c42763792a49" },
  { c: "hwgp8", p: 'main', t: 9.245, m: 71.0, sw: 32, dr: 22, tot: 54, birth: "2026-07-14T22:34:08Z", done: "2026-07-14T23:35:10Z", node: "i-09fdd03b5776ec630" },
  { c: "kgkmw", p: 'main', t: 9.477, m: 69.3, sw: 52, dr: 54, tot: 106, birth: "2026-07-14T22:46:24Z", done: "2026-07-14T23:49:06Z", node: "i-0fb0f63a1fa25c348" },
  { c: "mjwgw", p: 'main', t: 9.692, m: 70.5, sw: 40, dr: 48, tot: 88, birth: "2026-07-14T23:00:29Z", done: "2026-07-15T00:02:01Z", node: "i-067d3e9a600db37df" },
  { c: "jvjhr", p: 'main', t: 9.842, m: 71.2, sw: 24, dr: 22, tot: 46, birth: "2026-07-14T23:10:08Z", done: "2026-07-15T00:10:59Z", node: "i-0027a501d3d908495" },
  { c: "b9b96", p: 'main', t: 10.045, m: 71.0, sw: 31, dr: 23, tot: 54, birth: "2026-07-14T23:22:13Z", done: "2026-07-15T00:23:11Z", node: "i-0c08f4d5cd021455d" },
  { c: "rxf2c", p: 'main', t: 10.261, m: 70.1, sw: 25, dr: 80, tot: 105, birth: "2026-07-14T23:34:17Z", done: "2026-07-15T00:36:09Z", node: "i-020c3d914bdd495cf" },
  { c: "xv8m2", p: 'main', t: 10.472, m: 70.5, sw: 54, dr: 28, tot: 81, birth: "2026-07-14T23:47:22Z", done: "2026-07-15T00:48:49Z", node: "i-00564c34b6324ae46" },
  { c: "dxq8p", p: 'main', t: 10.714, m: 69.3, sw: 41, dr: 65, tot: 106, birth: "2026-07-15T00:00:35Z", done: "2026-07-15T01:03:19Z", node: "i-06ca63b87ebb447dc" },
  { c: "kbm2x", p: 'main', t: 10.846, m: 71.0, sw: 34, dr: 22, tot: 56, birth: "2026-07-15T00:10:14Z", done: "2026-07-15T01:11:15Z", node: "i-002e0c626c9965415" },
  { c: "lnlxb", p: 'main', t: 11.046, m: 71.1, sw: 27, dr: 28, tot: 55, birth: "2026-07-15T00:22:20Z", done: "2026-07-15T01:23:16Z", node: "i-07ed6412f48b14708" },
  { c: "kgj2d", p: 'main', t: 11.26, m: 70.3, sw: 25, dr: 73, tot: 97, birth: "2026-07-15T00:34:25Z", done: "2026-07-15T01:36:04Z", node: "i-084bb7cca02775ea4" },
  { c: "tr854", p: 'main', t: 11.476, m: 70.4, sw: 24, dr: 69, tot: 93, birth: "2026-07-15T00:47:30Z", done: "2026-07-15T01:49:04Z", node: "i-0be5128cc2cb31ace" },
  { c: "d94p8", p: 'main', t: 11.698, m: 71.2, sw: 27, dr: 18, tot: 45, birth: "2026-07-15T01:01:35Z", done: "2026-07-15T02:02:21Z", node: "i-01a1c010ed9bb8abc" },
  { c: "btlwz", p: 'main', t: 11.859, m: 70.3, sw: 31, dr: 34, tot: 64, birth: "2026-07-15T01:10:20Z", done: "2026-07-15T02:12:01Z", node: "i-0459aab876617973b" },
  { c: "nssls", p: 'main', t: 12.061, m: 70.2, sw: 23, dr: 62, tot: 85, birth: "2026-07-15T01:22:24Z", done: "2026-07-15T02:24:09Z", node: "i-019f5eed0b9c24fca" },
  { c: "xwpp9", p: 'main', t: 12.264, m: 70.2, sw: 42, dr: 48, tot: 90, birth: "2026-07-15T01:34:29Z", done: "2026-07-15T02:36:18Z", node: "i-0449ff069770389cb" },
  { c: "2fmbr", p: 'main', t: 12.484, m: 70.1, sw: 38, dr: 58, tot: 96, birth: "2026-07-15T01:47:34Z", done: "2026-07-15T02:49:30Z", node: "i-03b5247bae327d09a" },
  { c: "88rjv", p: 'main', t: 12.708, m: 70.7, sw: 26, dr: 33, tot: 59, birth: "2026-07-15T02:01:38Z", done: "2026-07-15T03:02:57Z", node: "i-0adbdb142e012b81d" },
  { c: "qdh2f", p: 'main', t: 12.874, m: 70.1, sw: 35, dr: 62, tot: 97, birth: "2026-07-15T02:10:59Z", done: "2026-07-15T03:12:55Z", node: "i-07fa42782b1206b04" },
  { c: "97mm5", p: 'main', t: 13.066, m: 70.3, sw: 24, dr: 39, tot: 63, birth: "2026-07-15T02:22:47Z", done: "2026-07-15T03:24:26Z", node: "i-0eb70b7e8fe3a72c5" },
  { c: "rbv8l", p: 'main', t: 13.265, m: 70.5, sw: 23, dr: 33, tot: 56, birth: "2026-07-15T02:34:52Z", done: "2026-07-15T03:36:23Z", node: "i-0d032d5175f8d1b1b" },
  { c: "77m27", p: 'main', t: 13.489, m: 70.1, sw: 35, dr: 42, tot: 77, birth: "2026-07-15T02:47:56Z", done: "2026-07-15T03:49:49Z", node: "i-01b785c0e28e3d678" },
  { c: "5qwn6", p: 'main', t: 13.73, m: 69.8, sw: 36, dr: 62, tot: 98, birth: "2026-07-15T03:02:01Z", done: "2026-07-15T04:04:16Z", node: "i-0651204cabf6506a8" },
  { c: "zv9gp", p: 'main', t: 13.896, m: 69.1, sw: 42, dr: 79, tot: 121, birth: "2026-07-15T03:11:20Z", done: "2026-07-15T04:14:16Z", node: "i-07d0545749cf4c983" },
  { c: "epi-gtx42", p: 'epi', t: 14.071, m: 10.1, sw: null, dr: 48, tot: null, birth: "2026-07-15T02:22:50Z", done: "2026-07-15T04:24:46Z", node: null },
  { c: "49pqr", p: 'main', t: 14.082, m: 70.0, sw: 33, dr: 53, tot: 87, birth: "2026-07-15T03:23:25Z", done: "2026-07-15T04:25:24Z", node: "i-0507f5b02688986aa" },
]
