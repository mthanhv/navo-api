-- Accent-insensitive search and sorting for Vietnamese (bug report 2026-11-20): "song tra" finds
-- "Sông Trà", and names starting with Đ sort with D instead of after Z. Self-contained (no unaccent
-- extension needed) and IMMUTABLE so it can back expression indexes later.
CREATE OR REPLACE FUNCTION vn_fold(t text) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$
    SELECT lower(translate(t, 'àáạảãằắặẳẵăầấậẩẫâèéẹẻẽềếệểễêìíịỉĩòóọỏõồốộổỗôờớợởỡơùúụủũừứựửữưỳýỵỷỹÀÁẠẢÃẰẮẶẲẴĂẦẤẬẨẪÂÈÉẸẺẼỀẾỆỂỄÊÌÍỊỈĨÒÓỌỎÕỒỐỘỔỖÔỜỚỢỞỠƠÙÚỤỦŨỪỨỰỬỮƯỲÝỴỶỸđĐ', 'aaaaaaaaaaaaaaaaaeeeeeeeeeeeiiiiiooooooooooooooooouuuuuuuuuuuyyyyyAAAAAAAAAAAAAAAAAEEEEEEEEEEEIIIIIOOOOOOOOOOOOOOOOOUUUUUUUUUUUYYYYYdD'))
$$;
