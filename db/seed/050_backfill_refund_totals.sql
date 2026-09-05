-- Keep the transaction's denormalised refund total consistent with the
-- disputes that were actually refunded. The CHECK constraint
-- (refunded_minor <= amount_minor) is what catches a generator bug here.

UPDATE transactions t
   SET refunded_minor = r.total,
       status = CASE WHEN r.total >= t.amount_minor THEN 'refunded' ELSE 'partially_refunded' END
  FROM (
        SELECT d.transaction_id, SUM(d.amount_minor) AS total
          FROM disputes d
         WHERE d.state = 'refunded'
         GROUP BY d.transaction_id
       ) AS r
 WHERE t.id = r.transaction_id;
