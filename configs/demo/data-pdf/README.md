# Sandbox data for the PDF act

The Workiva sandbox is seeded from this folder (`nl-demo-seed -data configs/demo/data-pdf`). It is `../data` with the electricity and Scope 2 figures of the synthetic supplier statement (Velnorq, VEN-ESG-2025-01847):

- Renewable electricity 2025 is blank: the analyst fills it in Teams from the statement (page 7, Table 5 total, 3,020 MWh). 2024: 2,450 MWh.
- Non-renewable electricity: 1,842.74 MWh (2024: 2,654.22), the unmatched balance of 4,862,740 kWh (page 6 annual total).
- Total energy consumption is the sum of the four electricity fields: 5,070.74 MWh once renewable is filled (2024: 5,304.22).
- Scope 2 location-based 1,507.4 tCO2e (2024: 1,735.4), market-based 350.1 (2024: 557.4), page 9.

`../data` stays the offline demo dataset with its planted findings and tests.
