/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type { SupplierAccount, SupplierTemplate } from '../types'

export function supplierChannelTemplates(
  supplier: SupplierAccount,
  catalog: SupplierTemplate[]
): SupplierTemplate[] {
  const allowed =
    supplier.allowed_templates ?? supplier.allowed_types.map(String)
  return catalog
    .filter((template) => allowed.includes(template.id))
    .map((template) => ({
      ...template,
      models: template.models.length
        ? template.models.filter((model) =>
            supplier.allowed_models.includes(model)
          )
        : supplier.allowed_models,
    }))
}
