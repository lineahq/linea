import type { Grouping } from '../api/types';

/** sampleGrouping is the grouping of samples/resource-grouping from a real run (groupbyattrs v0.162.0). */
export const sampleGrouping: Grouping = {
  "processor": "groupbyattrs",
  "pipeline": "logs",
  "signal": "logs",
  "keys": [
    "host.name"
  ],
  "requests": [
    {
      "input": 4,
      "output": 5,
      "before": [
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "checkout"
            }
          ],
          "records": 4
        },
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "cart"
            }
          ],
          "records": 1
        },
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "checkout"
            }
          ],
          "records": 1
        }
      ],
      "after": [
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "checkout"
            },
            {
              "key": "host.name",
              "value": "node-1",
              "moved": true
            }
          ],
          "records": [
            {
              "lineage": "3:resource[0].scope[0].log_record[0]",
              "name": "order 1 placed",
              "from": 0,
              "moved": [
                "host.name"
              ]
            },
            {
              "lineage": "3:resource[0].scope[0].log_record[3]",
              "name": "order 3 placed",
              "from": 0,
              "moved": [
                "host.name"
              ]
            }
          ],
          "from": [
            {
              "block": 0,
              "records": 2
            }
          ]
        },
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "checkout"
            },
            {
              "key": "host.name",
              "value": "node-2",
              "moved": true
            }
          ],
          "records": [
            {
              "lineage": "3:resource[0].scope[0].log_record[1]",
              "name": "order 2 placed",
              "from": 0,
              "moved": [
                "host.name"
              ]
            }
          ],
          "from": [
            {
              "block": 0,
              "records": 1
            }
          ]
        },
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "checkout"
            }
          ],
          "records": [
            {
              "lineage": "3:resource[0].scope[0].log_record[2]",
              "name": "cache warmed",
              "from": 0
            },
            {
              "lineage": "3:resource[2].scope[0].log_record[0]",
              "name": "checkout started",
              "from": 2
            }
          ],
          "from": [
            {
              "block": 0,
              "records": 1
            },
            {
              "block": 2,
              "records": 1
            }
          ]
        },
        {
          "attributes": [
            {
              "key": "service.name",
              "value": "cart"
            },
            {
              "key": "host.name",
              "value": "node-1",
              "moved": true
            }
          ],
          "records": [
            {
              "lineage": "3:resource[1].scope[0].log_record[0]",
              "name": "cart 7 updated",
              "from": 1,
              "moved": [
                "host.name"
              ]
            }
          ],
          "from": [
            {
              "block": 1,
              "records": 1
            }
          ]
        }
      ],
      "moved": [
        "host.name"
      ],
      "grouped": 4,
      "kept": 2,
      "unfollowed": 0
    }
  ]
};
