# WAVE9 C4 fix 1 visual comparison

Preflight measured all 63 C4 renders at 390, 768 and 1440 CSS px in EN, AR and ZH. F3 references come from the approved export manifest and PROMOTE tablet index. The preflight served the current Astro output with the previously verified image assets. Full-band differences over the gate are bold. **21 headers and 44 footers exceed 2%.**

Pixel difference counts pixels with a change greater than 10 in any RGB channel. The images use native CSS pixels, without scaling or image registration. Header and full page align at the top, footer aligns at the bottom. Rows missing from either image count as changed. The last two columns reproduce the independent review’s 80px top and 200px bottom strips. Full-page pixel differences include cumulative vertical displacement.

| Page | Locale | Width | Full page | Header band | Footer band | Top 80px | Bottom 200px |
|---|---|---:|---:|---:|---:|---:|---:|
| 404 | ar | 390 | 11.24% | 1.67% | **3.92%** | 1.86% | 2.62% |
| 404 | ar | 768 | 22.01% | 0.85% | **2.45%** | 0.94% | 1.46% |
| 404 | ar | 1440 | 3.70% | **2.70%** | **2.19%** | 3.00% | 1.05% |
| 404 | en | 390 | 13.30% | 1.75% | **4.58%** | 1.95% | 2.47% |
| 404 | en | 768 | 7.87% | 0.89% | **2.61%** | 0.98% | 1.26% |
| 404 | en | 1440 | 3.26% | **3.25%** | 1.44% | 3.62% | 0.94% |
| 404 | zh | 390 | 15.64% | 1.78% | **4.04%** | 1.98% | 1.57% |
| 404 | zh | 768 | 15.47% | 0.90% | **2.56%** | 1.01% | 0.93% |
| 404 | zh | 1440 | 4.50% | **3.58%** | 1.01% | 3.98% | 0.67% |
| about-us | ar | 390 | 26.64% | 1.67% | **4.74%** | 1.86% | 3.77% |
| about-us | ar | 768 | 27.61% | 0.85% | **2.72%** | 0.94% | 1.89% |
| about-us | ar | 1440 | 36.97% | **3.28%** | **2.29%** | 3.65% | 1.12% |
| about-us | en | 390 | 26.38% | 1.75% | **5.37%** | 1.95% | 4.10% |
| about-us | en | 768 | 19.93% | 0.88% | **2.31%** | 0.98% | 1.46% |
| about-us | en | 1440 | 14.92% | **3.25%** | 1.74% | 3.62% | 1.11% |
| about-us | zh | 390 | 24.49% | 1.78% | **4.95%** | 1.98% | 3.82% |
| about-us | zh | 768 | 21.09% | 0.90% | **2.45%** | 1.00% | 1.67% |
| about-us | zh | 1440 | 13.38% | **3.58%** | 1.54% | 3.98% | 0.98% |
| oem-odm | ar | 390 | 55.68% | 1.67% | **3.84%** | 1.86% | 2.55% |
| oem-odm | ar | 768 | 37.27% | 0.85% | **2.44%** | 0.94% | 1.47% |
| oem-odm | ar | 1440 | 30.28% | **2.91%** | **2.19%** | 3.23% | 1.05% |
| oem-odm | en | 390 | 53.49% | 1.75% | **3.92%** | 1.95% | 2.47% |
| oem-odm | en | 768 | 38.46% | 0.88% | **2.61%** | 0.98% | 1.26% |
| oem-odm | en | 1440 | 20.25% | **3.25%** | 1.47% | 3.62% | 0.95% |
| oem-odm | zh | 390 | 49.98% | 1.78% | **2.73%** | 1.98% | 1.57% |
| oem-odm | zh | 768 | 38.68% | 0.90% | 1.84% | 1.00% | 0.93% |
| oem-odm | zh | 1440 | 19.66% | **3.58%** | 1.02% | 3.98% | 0.67% |
| privacy | ar | 390 | 15.74% | 1.67% | **3.84%** | 1.86% | 2.55% |
| privacy | ar | 768 | 10.00% | 0.85% | **2.44%** | 0.95% | 1.47% |
| privacy | ar | 1440 | 9.94% | **2.86%** | **2.19%** | 3.18% | 1.05% |
| privacy | en | 390 | 17.19% | 1.75% | **3.92%** | 1.94% | 2.47% |
| privacy | en | 768 | 10.49% | 0.89% | **2.19%** | 0.99% | 1.26% |
| privacy | en | 1440 | 8.63% | **3.25%** | 1.47% | 3.62% | 0.95% |
| privacy | zh | 390 | 18.52% | 1.78% | **2.73%** | 1.98% | 1.57% |
| privacy | zh | 768 | 12.08% | 0.91% | 1.86% | 1.02% | 0.92% |
| privacy | zh | 1440 | 8.23% | **3.57%** | 1.02% | 3.97% | 0.67% |
| sampling | ar | 390 | 36.40% | 1.67% | **3.92%** | 1.86% | 2.62% |
| sampling | ar | 768 | 15.94% | 0.85% | **2.45%** | 0.94% | 1.46% |
| sampling | ar | 1440 | 7.83% | **2.90%** | **2.19%** | 3.23% | 1.05% |
| sampling | en | 390 | 31.51% | 1.75% | **4.00%** | 1.94% | 2.50% |
| sampling | en | 768 | 21.63% | 0.88% | **2.19%** | 0.98% | 1.26% |
| sampling | en | 1440 | 3.19% | **3.25%** | 1.47% | 3.62% | 0.95% |
| sampling | zh | 390 | 33.68% | 1.78% | **2.69%** | 1.98% | 1.57% |
| sampling | zh | 768 | 16.93% | 0.90% | 1.86% | 1.00% | 0.92% |
| sampling | zh | 1440 | 2.73% | **3.58%** | 1.01% | 3.98% | 0.67% |
| shipping | ar | 390 | 12.89% | 1.67% | **3.92%** | 1.86% | 2.62% |
| shipping | ar | 768 | 23.05% | 0.85% | **2.45%** | 0.95% | 1.46% |
| shipping | ar | 1440 | 2.72% | **2.90%** | **2.19%** | 3.23% | 1.05% |
| shipping | en | 390 | 11.09% | 1.75% | **3.92%** | 1.95% | 2.47% |
| shipping | en | 768 | 21.05% | 0.89% | **2.19%** | 0.98% | 1.26% |
| shipping | en | 1440 | 2.49% | **3.25%** | 1.44% | 3.62% | 0.94% |
| shipping | zh | 390 | 12.18% | 1.78% | **2.69%** | 1.98% | 1.57% |
| shipping | zh | 768 | 18.13% | 0.91% | 1.86% | 1.01% | 0.92% |
| shipping | zh | 1440 | 2.02% | **3.58%** | 1.02% | 3.98% | 0.67% |
| terms | ar | 390 | 15.83% | 1.67% | **3.84%** | 1.86% | 2.55% |
| terms | ar | 768 | 7.66% | 0.85% | **2.44%** | 0.95% | 1.47% |
| terms | ar | 1440 | 8.42% | **2.86%** | **2.19%** | 3.18% | 1.05% |
| terms | en | 390 | 19.00% | 1.75% | **3.92%** | 1.95% | 2.47% |
| terms | en | 768 | 9.75% | 0.89% | **2.19%** | 0.99% | 1.26% |
| terms | en | 1440 | 9.75% | **3.25%** | 1.47% | 3.62% | 0.95% |
| terms | zh | 390 | 18.18% | 1.78% | **2.69%** | 1.98% | 1.57% |
| terms | zh | 768 | 8.86% | 0.91% | 1.84% | 1.02% | 0.93% |
| terms | zh | 1440 | 8.85% | **3.57%** | 1.01% | 3.97% | 0.67% |

## Section height differences over 3%

The table separates vertical insets from the interior so residual differences are traceable to the frame and rendered box. Interior includes children, their wrapping and vertical gaps. Positive differences mean the rendered box is taller.

| Page/locale/width | F3 section (node) | F3 px | Rendered px | Change | Insets Δ px | Interior Δ px |
|---|---|---:|---:|---:|---:|---:|
| 404/ar/390 | MGOO / centered recovery (1472:266912) | 905.0 | 868.6 | -4.0% | -40.0 | +3.6 |
| 404/ar/768 | MGOO / centered recovery (1493:235181) | 789.0 | 916.6 | +16.2% | +8.0 | +119.6 |
| 404/en/390 | MGOO / centered recovery (1472:266886) | 789.0 | 746.5 | -5.4% | -40.0 | -2.5 |
| 404/zh/768 | MGOO / centered recovery (1493:235292) | 789.0 | 827.5 | +4.9% | +8.0 | +30.5 |
| about-us/ar/390 | About / RTL / 06 Orders and shipping / 390 (1472:264472) | 810.0 | 758.5 | -6.4% | +0.0 | -51.5 |
| about-us/ar/768 | About / RTL / 01 Who we are / 390 (1493:226148) | 1212.5 | 1249.2 | +3.0% | +0.0 | +36.7 |
| about-us/ar/768 | About / RTL / 03 Ways to work / 390 (1493:226176) | 1101.0 | 1020.3 | -7.3% | +0.0 | -80.7 |
| about-us/ar/768 | About / RTL / 06 Orders and shipping / 390 (1493:226218) | 760.0 | 659.3 | -13.3% | +0.0 | -100.7 |
| about-us/ar/1440 | About / RTL / 01 Who we are / 1440 (1472:264305) | 1263.0 | 1315.6 | +4.2% | +0.0 | +52.6 |
| about-us/ar/1440 | About / RTL / 03 Ways to work / 1440 (1472:264333) | 639.0 | 688.1 | +7.7% | +0.0 | +49.1 |
| about-us/ar/1440 | About / RTL / 04 Branding and sampling / 1440 (1472:264353) | 542.0 | 575.1 | +6.1% | +0.0 | +33.1 |
| about-us/ar/1440 | About / RTL / 06 Orders and shipping / 1440 (1472:264375) | 523.0 | 545.8 | +4.4% | +0.0 | +22.8 |
| about-us/ar/1440 | About / RTL / 08 Start your order / 1440 (1472:264393) | 635.0 | 669.2 | +5.4% | +0.0 | +34.2 |
| about-us/en/390 | About2 / 05 Guangzhou stall and factory / 390 (1472:264266) | 957.0 | 919.3 | -3.9% | +0.0 | -37.7 |
| about-us/en/390 | About2 / 06 Orders and shipping / 390 (1472:264278) | 840.0 | 787.3 | -6.3% | +0.0 | -52.7 |
| about-us/en/768 | About2 / 06 Orders and shipping / 390 (1493:225856) | 760.0 | 658.3 | -13.4% | +0.0 | -101.7 |
| about-us/zh/390 | About2 / 03 Ways to work / 390 (1472:264624) | 951.0 | 980.8 | +3.1% | +0.0 | +29.8 |
| about-us/zh/390 | About2 / 06 Orders and shipping / 390 (1472:264666) | 735.0 | 683.1 | -7.1% | +0.0 | -51.9 |
| about-us/zh/768 | About2 / 03 Ways to work / 390 (1493:225995) | 851.0 | 881.6 | +3.6% | +0.0 | +30.6 |
| about-us/zh/768 | About2 / 06 Orders and shipping / 390 (1493:226037) | 710.0 | 608.7 | -14.3% | +0.0 | -101.3 |
| oem-odm/ar/390 | 02 / Centered service proposition (1472:262537) | 350.0 | 402.8 | +15.1% | +0.0 | +52.8 |
| oem-odm/ar/390 | 03 / Three capabilities (1472:262542) | 677.0 | 730.6 | +7.9% | +0.0 | +53.6 |
| oem-odm/ar/390 | 04 / Confirmed customization capability matrix (1472:262559) | 1533.0 | 1860.2 | +21.3% | +0.0 | +327.2 |
| oem-odm/ar/390 | 05 / Three image-led workflow stages (1472:262591) | 1709.0 | 1762.6 | +3.1% | +0.0 | +53.6 |
| oem-odm/ar/390 | 07 / Sixteen root-approved technique studies (1472:262618) | 2676.0 | 2531.2 | -5.4% | +0.0 | -144.8 |
| oem-odm/ar/768 | 07 / Sixteen root-approved technique studies (1493:222152) | 2663.0 | 2468.0 | -7.3% | +0.0 | -195.0 |
| oem-odm/en/390 | 02 / Centered service proposition (1472:262296) | 350.0 | 402.0 | +14.8% | +0.0 | +52.0 |
| oem-odm/en/390 | 03 / Three capabilities (1472:262301) | 677.0 | 707.2 | +4.5% | +0.0 | +30.2 |
| oem-odm/en/390 | 04 / Confirmed customization capability matrix (1472:262318) | 1539.0 | 1655.4 | +7.6% | +0.0 | +116.4 |
| oem-odm/en/390 | 05 / Three image-led workflow stages (1472:262350) | 1709.0 | 1777.6 | +4.0% | +0.0 | +68.6 |
| oem-odm/en/768 | 03 / Three capabilities (1493:221009) | 716.0 | 668.8 | -6.6% | +0.0 | -47.2 |
| oem-odm/en/768 | 06 / Assurance and inquiry brief (1493:221137) | 644.0 | 624.3 | -3.1% | +0.0 | -19.7 |
| oem-odm/zh/390 | 02 / Centered service proposition (1472:262778) | 350.0 | 363.6 | +3.9% | +0.0 | +13.6 |
| oem-odm/zh/390 | 03 / Three capabilities (1472:262783) | 646.0 | 707.2 | +9.5% | +0.0 | +61.2 |
| oem-odm/zh/390 | 04 / Confirmed customization capability matrix (1472:262800) | 1408.0 | 1450.6 | +3.0% | +0.0 | +42.6 |
| oem-odm/zh/390 | 05 / Three image-led workflow stages (1472:262832) | 1678.0 | 1739.2 | +3.6% | +0.0 | +61.2 |
| oem-odm/zh/390 | 07 / Sixteen root-approved technique studies (1472:262859) | 2451.0 | 2291.3 | -6.5% | +0.0 | -159.7 |
| oem-odm/zh/768 | 07 / Sixteen root-approved technique studies (1493:221695) | 2463.0 | 2291.3 | -7.0% | +0.0 | -171.7 |
| privacy/ar/1440 | MGOO / single reading surface (1472:268147) | 1405.0 | 1448.2 | +3.1% | +0.0 | +43.2 |
| sampling/ar/390 | Capabilities / Sample your finishing details (1472:265632) | 397.0 | 507.3 | +27.8% | +0.0 | +110.3 |
| sampling/ar/768 | 01 / Centered process opening (1493:222747) | 266.0 | 253.7 | -4.6% | +0.0 | -12.3 |
| sampling/ar/768 | Capabilities / Sample your finishing details (1493:222770) | 402.0 | 435.9 | +8.4% | +32.0 | +1.9 |
| sampling/ar/768 | 05 / Seasonal planning (1493:222841) | 294.0 | 281.6 | -4.2% | +0.0 | -12.4 |
| sampling/ar/1440 | Capabilities / Sample your finishing details (1472:264813) | 321.0 | 337.0 | +5.0% | +0.0 | +16.0 |
| sampling/en/390 | 01 / Centered process opening (1472:264750) | 285.0 | 299.3 | +5.0% | +0.0 | +14.3 |
| sampling/en/390 | Capabilities / Sample your finishing details (1472:264754) | 397.0 | 486.6 | +22.6% | +0.0 | +89.6 |
| sampling/en/390 | 02 / Phase 01 (1472:264773) | 720.0 | 692.1 | -3.9% | +0.0 | -27.9 |
| sampling/en/390 | 05 / Seasonal planning (1472:264797) | 369.0 | 382.9 | +3.8% | +0.0 | +13.9 |
| sampling/en/768 | 01 / Centered process opening (1493:222349) | 309.0 | 299.3 | -3.2% | +0.0 | -9.7 |
| sampling/en/768 | Capabilities / Sample your finishing details (1493:222372) | 402.0 | 416.1 | +3.5% | +32.0 | -17.9 |
| sampling/en/768 | 02 / Phase 01 (1493:222399) | 761.0 | 720.9 | -5.3% | +0.0 | -40.1 |
| sampling/zh/390 | 01 / Centered process opening (1472:265744) | 254.0 | 299.3 | +17.8% | +0.0 | +45.3 |
| sampling/zh/390 | Capabilities / Sample your finishing details (1472:265748) | 397.0 | 433.7 | +9.2% | +0.0 | +36.7 |
| sampling/zh/768 | Capabilities / Sample your finishing details (1493:222571) | 402.0 | 416.1 | +3.5% | +32.0 | -17.9 |
| shipping/ar/390 | 01 / Centered process opening (1472:265962) | 285.0 | 299.3 | +5.0% | +0.0 | +14.3 |
| shipping/ar/390 | 02 / Phase 01 (1472:265966) | 720.0 | 692.1 | -3.9% | +0.0 | -27.9 |
| shipping/ar/390 | 04 / Phase 03 (1472:265982) | 720.0 | 692.1 | -3.9% | +0.0 | -27.9 |
| shipping/ar/390 | 05 / Service information (1472:265990) | 646.0 | 692.2 | +7.2% | +0.0 | +46.2 |
| shipping/ar/768 | 02 / Phase 01 (1493:223332) | 761.0 | 720.9 | -5.3% | +0.0 | -40.1 |
| shipping/ar/768 | 04 / Phase 03 (1493:223364) | 761.0 | 720.9 | -5.3% | +0.0 | -40.1 |
| shipping/en/390 | 01 / Centered process opening (1472:265856) | 313.0 | 327.2 | +4.5% | +0.0 | +14.2 |
| shipping/en/768 | 01 / Centered process opening (1493:222946) | 309.0 | 299.3 | -3.2% | +0.0 | -9.7 |
| shipping/en/768 | 05 / Service information (1493:223015) | 723.0 | 668.8 | -7.5% | +0.0 | -54.2 |
| shipping/zh/390 | 05 / Service information (1472:266848) | 646.0 | 668.8 | +3.5% | +0.0 | +22.8 |
