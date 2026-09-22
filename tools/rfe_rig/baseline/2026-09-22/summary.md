# RFE validation rig summary

Results directory: `/mnt/data/rfe-val/rig/results/2026-09-22`

## Answer language (#964 port)

- 20 of 24 answers in the language asked (2 run(s) per question)
- 0 of 24 answers carry a citation tag
- 0 undecidable by the detector, 0 errors
- 24 of 24 answers were retrieved-context fallbacks (generator failed): the language score reflects the retrieved chunks, not generated answers
  - MISS want=en got=ru run=1: When did Dmytro Kuleba become Minister of Foreign Affairs?
  - MISS want=en got=ru run=2: When did Dmytro Kuleba become Minister of Foreign Affairs?
  - MISS want=ky got=ru run=1: Садыр Жапаров референдум жөнүндө эмне дейт?
  - MISS want=ky got=ru run=2: Садыр Жапаров референдум жөнүндө эмне дейт?

Replay of stored answers `ask_2026-09-12_main-final.json`: 24 of 24 in the language asked, 20 of 24 with a citation tag.

## Coverage baseline (dir2mcp state)

| recording | language | source | conf | 8.6.13 coverage | language_covered | live chunks | deleted | window mean s | window max s | delivered s | duration s | delivered % | pre-#955 delivered % |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| geo_2007_kikabidze_interview.flac | ru | detected | 1.00 | absent | absent | 46 | 153 | 33.3 | 46.8 | 1531.9 | 1545.3 | 99.1% | 78.6% |
| kgz_18448_extremism_hospital.flac | - | - | - | absent | absent | 50 | 260 | 36.7 | 92.2 | 1837.3 | 1877.0 | 97.9% | 83.8% |
| kgz_21138_japarov_broll.flac | mk | detected | 1.00 | absent | absent | 126 | 411 | 34.7 | 96.0 | 4369.6 | 4384.6 | 99.7% | 81.0% |
| kgz_29216_rahat_interview.wav | tk | detected | 1.00 | absent | absent | 55 | 227 | 33.6 | 40.1 | 1849.1 | 1851.6 | 99.9% | 81.9% |
| rus_28974_shalygina_interview.flac | ru | detected | 0.74 | absent | absent | 50 | 232 | 34.6 | 45.8 | 1728.6 | 1731.5 | 99.8% | 81.1% |
| ukr_0652_filaret_programme.flac | uk | detected | 1.00 | absent | absent | 60 | 125 | 33.1 | 39.6 | 1986.2 | 2001.2 | 99.3% | 69.4% |
| ukr_1108_kravchuk_interview.flac | ru | detected | 1.00 | absent | absent | 55 | 162 | 33.4 | 39.9 | 1837.1 | 1841.0 | 99.8% | 81.7% |
| ukr_18126_kuleba_interview.flac | en | detected | 1.00 | absent | absent | 45 | 180 | 36.0 | 39.9 | 1620.7 | 1622.8 | 99.9% | 75.1% |

## Cross-model agreement

| recording | A (reference) | B | windows | speech | agreeing | agree all | coverage by agreement | mean WER | overall WER | mean NED | A tokens | B tokens |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| geo_2007_kikabidze_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-76d56b0863da | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | 52 | 52 | 49 | 94.2% | 94.2% | 0.255 | 0.236 | 0.219 | 2793 | 2967 |
| geo_2007_kikabidze_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-76d56b0863da | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | 52 | 52 | 26 | 50.0% | 50.0% | 0.592 | 0.574 | 0.562 | 2793 | 2595 |
| geo_2007_kikabidze_interview.flac | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | 52 | 52 | 27 | 51.9% | 51.9% | 0.554 | 0.548 | 0.552 | 2967 | 2595 |
| kgz_18448_extremism_hospital.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-b366b6e981c7 | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | 63 | 63 | 0 | 0.0% | 0.0% | 1.095 | 0.994 | 0.911 | 3023 | 2988 |
| kgz_18448_extremism_hospital.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-b366b6e981c7 | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | 63 | 63 | 0 | 0.0% | 0.0% | 1.173 | 1.084 | 0.907 | 3023 | 3496 |
| kgz_18448_extremism_hospital.flac | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | 63 | 63 | 18 | 28.6% | 28.6% | 0.603 | 0.582 | 0.593 | 3496 | 2988 |
| kgz_21138_japarov_broll.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-58f8920d30cd | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | 147 | 147 | 0 | 0.0% | 0.0% | 1.709 | 1.084 | 0.935 | 7199 | 7587 |
| kgz_21138_japarov_broll.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-58f8920d30cd | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | 147 | 147 | 0 | 0.0% | 0.0% | 2.450 | 1.202 | 0.930 | 7199 | 9030 |
| kgz_21138_japarov_broll.flac | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | 147 | 147 | 63 | 42.9% | 42.9% | 0.759 | 0.547 | 0.568 | 9030 | 7587 |
| kgz_29216_rahat_interview.wav | dir2mcp-whisper-large-v3-multi__large-v3-multi-22bf375577f5 | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | 62 | 62 | 0 | 0.0% | 0.0% | 1.110 | 0.960 | 0.908 | 3404 | 3143 |
| kgz_29216_rahat_interview.wav | dir2mcp-whisper-large-v3-multi__large-v3-multi-22bf375577f5 | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | 62 | 62 | 0 | 0.0% | 0.0% | 1.244 | 1.048 | 0.896 | 3404 | 3893 |
| kgz_29216_rahat_interview.wav | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | 62 | 62 | 21 | 33.9% | 33.9% | 0.582 | 0.576 | 0.575 | 3893 | 3143 |
| rus_28974_shalygina_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-dbe07da33c68 | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | 58 | 58 | 49 | 84.5% | 84.5% | 0.370 | 0.295 | 0.283 | 3154 | 3528 |
| rus_28974_shalygina_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-dbe07da33c68 | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | 58 | 58 | 22 | 37.9% | 37.9% | 0.611 | 0.560 | 0.572 | 3154 | 2865 |
| rus_28974_shalygina_interview.flac | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | 58 | 58 | 22 | 37.9% | 37.9% | 0.589 | 0.542 | 0.576 | 3528 | 2865 |
| ukr_0652_filaret_programme.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-6876a1b269fe | mms-1b-all-ukr__ukr-3d33597edbda-tf5.13.1 | 67 | 67 | 64 | 95.5% | 95.5% | 0.274 | 0.270 | 0.271 | 2755 | 2620 |
| ukr_1108_kravchuk_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-5ce9e2685163 | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | 62 | 62 | 61 | 98.4% | 98.4% | 0.208 | 0.200 | 0.189 | 2920 | 3055 |
| ukr_1108_kravchuk_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-5ce9e2685163 | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | 62 | 62 | 47 | 75.8% | 75.8% | 0.417 | 0.420 | 0.412 | 2920 | 2597 |
| ukr_1108_kravchuk_interview.flac | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | 62 | 62 | 46 | 74.2% | 74.2% | 0.440 | 0.447 | 0.438 | 3055 | 2597 |
| ukr_18126_kuleba_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-6a821246c5cc | mms-1b-all-ukr__ukr-3d33597edbda-tf5.13.1 | 55 | 55 | 34 | 61.8% | 61.8% | 0.556 | 0.581 | 0.541 | 3263 | 2696 |

## Transcripts on file

| recording | decoder | kind | language | segments | words | chars |
|---|---|---|---|---|---|---|
| geo_2007_kikabidze_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-76d56b0863da | sqlite | ru | 46 | 2801 | 16908 |
| geo_2007_kikabidze_interview.flac | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | http | ru | 194 | 2942 | 17955 |
| geo_2007_kikabidze_interview.flac | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | mms | rus | 52 | 2581 | 15591 |
| kgz_18448_extremism_hospital.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-b366b6e981c7 | sqlite | - | 50 | 3023 | 21110 |
| kgz_18448_extremism_hospital.flac | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | mms | kir | 63 | 2984 | 21570 |
| kgz_18448_extremism_hospital.flac | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | fw | kk | 63 | 3496 | 23422 |
| kgz_21138_japarov_broll.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-58f8920d30cd | sqlite | mk | 126 | 7206 | 50407 |
| kgz_21138_japarov_broll.flac | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | mms | kir | 147 | 7549 | 56661 |
| kgz_21138_japarov_broll.flac | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | fw | kk | 162 | 9031 | 61212 |
| kgz_29216_rahat_interview.wav | dir2mcp-whisper-large-v3-multi__large-v3-multi-22bf375577f5 | sqlite | tk | 55 | 3408 | 24480 |
| kgz_29216_rahat_interview.wav | mms-1b-all-kir__kir-3d33597edbda-tf5.13.1 | mms | kir | 62 | 3121 | 23448 |
| kgz_29216_rahat_interview.wav | whisper-small-kyrgyz-ct2__small-kyrgyz-ct2-kk-fw1.2.1 | fw | kk | 70 | 3893 | 26444 |
| rus_28974_shalygina_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-dbe07da33c68 | sqlite | ru | 50 | 3154 | 18279 |
| rus_28974_shalygina_interview.flac | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | http | ru | 212 | 3473 | 20451 |
| rus_28974_shalygina_interview.flac | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | mms | rus | 58 | 2845 | 17057 |
| ukr_0652_filaret_programme.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-6876a1b269fe | sqlite | uk | 60 | 2773 | 16903 |
| ukr_0652_filaret_programme.flac | mms-1b-all-ukr__ukr-3d33597edbda-tf5.13.1 | mms | ukr | 67 | 2619 | 16105 |
| ukr_1108_kravchuk_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-5ce9e2685163 | sqlite | ru | 55 | 2938 | 18456 |
| ukr_1108_kravchuk_interview.flac | gigaam-v3-e2e_rnnt__gigaam-v3-e2e_rnnt | http | ru | 236 | 3059 | 19121 |
| ukr_1108_kravchuk_interview.flac | mms-1b-all-rus__rus-3d33597edbda-tf5.13.1 | mms | rus | 62 | 2592 | 16411 |
| ukr_18126_kuleba_interview.flac | dir2mcp-whisper-large-v3-multi__large-v3-multi-6a821246c5cc | sqlite | en | 45 | 3278 | 20299 |
| ukr_18126_kuleba_interview.flac | mms-1b-all-ukr__ukr-3d33597edbda-tf5.13.1 | mms | ukr | 55 | 2692 | 18133 |
