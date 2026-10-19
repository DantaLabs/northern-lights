# Demo ESG data

All values are invented for the fictional company in `../company.md`.

## Files

- `esg-2025.csv`, `esg-2024.csv`: header `key,label,unit,value`; same keys, labels, units and order in both. Environmental fields first, then social. An empty `value` is a missing value.
- `totals.csv`: `total_key,part_keys`, parts joined by `+`.

## Consistency rules

- Every total equals exactly the sum of its parts in both years.
- `energy_total` is the sum of the four mapped electricity parts only. The template adds a fuels part as a formula that is not mapped, so fuels are counted as zero in this data.
- Every change from 2024 to 2025 is below the materiality threshold of 10 percent, except the planted findings. No value changes sign.
- `fatalities_work_related` is 0 in both years, so its direction is `new` and it is never a finding.

## Planted findings (2025)

- `missing` `water_discharge_production` "Water discharge from undertaking production processes (m³)": 2024 14200, 2025 empty; the recipe marks it required.
- `material_change` `ghg_scope3_business_travel` "6. Business Travel": 2024 140, 2025 165 (+17.9%).
- `material_change` `training_hours_female` "Number of annual training hours per employee - Female": 2024 22.0, 2025 25.5 (+15.9%).
- `out_of_range` `electricity_renewable` "Electricity (as reflected in utility billings) - Renewable": 2024 520, 2025 490, recipe `min` 500 (change -5.8%, below the threshold).
