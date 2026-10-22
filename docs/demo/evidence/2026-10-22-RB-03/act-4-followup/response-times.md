# Act 4 follow-up response times

Measured from sending each prompt until the final answer settled. Operator review and gaps between prompts are excluded. Timing is informational; Tom removed the eight-minute acceptance gate.

| Prompt | Attempt | Sent (UTC) | Settled (UTC) | Seconds | Evidence |
|---|---:|---|---|---:|---|
| 4.1a | 0 | 2026-10-22T10:08:21.675415+00:00 | 2026-10-22T10:08:50.887744+00:00 | 29.212 | prompt-4.1a-round0.txt |
| 4.2a | 0 | 2026-10-22T10:09:06.085585+00:00 | 2026-10-22T10:09:30.704860+00:00 | 24.619 | prompt-4.2a-round0.txt |
| 4.3a | 0 | 2026-10-22T10:10:16.815676+00:00 | 2026-10-22T10:10:58.231766+00:00 | 41.416 | prompt-4.3a-round0.txt |
| 4.3b | 0 | 2026-10-22T10:14:26.923941+00:00 | 2026-10-22T10:18:00.522714+00:00 | 213.599 | prompt-4.3b-round0.txt |
| 4.3b | 1 | 2026-10-22T10:18:46.295510+00:00 | 2026-10-22T10:26:49.762799+00:00 | 483.467 | prompt-4.3b-round1.txt |
| 4.3b | 2 | 2026-10-22T10:29:49.728535+00:00 | 2026-10-22T10:38:07.228591+00:00 | 497.500 | prompt-4.3b-round2.txt |
| 4.3c | 0 | 2026-10-22T10:40:16.725602+00:00 | 2026-10-22T10:50:45.132484+00:00 | 628.407 | prompt-4.3c-round0.txt |
| 4.3d | 0 | 2026-10-22T10:50:56.911657+00:00 | 2026-10-22T11:03:20.027664+00:00 | 743.116 | prompt-4.3d-round0.txt |
| 4.3e | 0 | 2026-10-22T11:03:31.755851+00:00 | 2026-10-22T11:17:17.813204+00:00 | 826.057 | prompt-4.3e-round0.txt |
| 4.3f | 0 | 2026-10-22T11:17:27.722315+00:00 | 2026-10-22T11:32:44.481164+00:00 | 916.759 | prompt-4.3f-round0.txt |
| 4.3g | 0 | 2026-10-22T11:33:44.183982+00:00 | 2026-10-22T11:49:58.105577+00:00 | 973.922 | prompt-4.3g-round0.txt |
| 4.3h | 0 | 2026-10-22T11:53:55.461496+00:00 | NOT SHOWN | NOT SHOWN | parallel-interruption.json |
| 4.3j | 0 | 2026-10-22T11:53:55.927401+00:00 | 2026-10-22T11:54:33.748387+00:00 | 37.821 | prompt-4.3j-round0.txt |
| 4.3i | 0 | 2026-10-22T11:54:33.782855+00:00 | NOT SHOWN | NOT SHOWN | parallel-interruption.json |

## Prompts over 30 seconds

- 4.3a attempt 0: 41.416 seconds.
- 4.3b attempt 0: 213.599 seconds.
- 4.3b attempt 1: 483.467 seconds.
- 4.3b attempt 2: 497.500 seconds.
- 4.3c attempt 0: 628.407 seconds.
- 4.3d attempt 0: 743.116 seconds.
- 4.3e attempt 0: 826.057 seconds.
- 4.3f attempt 0: 916.759 seconds.
- 4.3g attempt 0: 973.922 seconds.
- 4.3j attempt 0: 37.821 seconds.

## Interrupted prompts

- 4.3h attempt 0: no settled answer observed; waited 1132.170 seconds through 2026-10-22T12:12:47.631684+00:00. See parallel-interruption.json. This is observed wait, not a settled response time.
- 4.3i attempt 0: no settled answer observed; waited 1093.849 seconds through 2026-10-22T12:12:47.631684+00:00. See parallel-interruption.json. This is observed wait, not a settled response time.

Raw timestamps and the exact redacted prompts are in [response-times.json](response-times.json).
