# hippocampus-gen

Test data generator for Hippocampus. These programs will create sample or test data for use with the [Hippocampus](https://github.com/fastbean-au/hippocampus) service.

## Random

The data produced by this utility is not meant to be particularly meaningful.

With this data generator, a wordlist is used to generate the event names, descriptions and bodies of memories. The wordlist used is the [MIT 10000 word list](https://www.mit.edu/~ecprice/wordlist.10000). The data itself is not meant to be particularly meaningful. However, this generator can be used to load test the Hippocampus service.

### Usage example

```bash
% go run cmd/random/main.go -e 135 -m 12000 -l 284 -p 13 -w 7
Starting worker: events (memories): 20 (223), memories 1492
Starting worker: events (memories): 19 (223), memories 1491
Starting worker: events (memories): 19 (223), memories 1491
Starting worker: events (memories): 19 (223), memories 1491
Starting worker: events (memories): 20 (223), memories 1492
Starting worker: events (memories): 19 (223), memories 1492
Starting worker: events (memories): 19 (222), memories 1491
```

## Book

The data produced by this utility is somewhat more useful in that it is not entirely meaningless data. The data used comes from the Charles Dickens novel Great Expectations.

This data generator uses the chapters as events, and each paragraph as a memory. The dates will increase chapter by chapter, paragraph by paragraph - this, obviously, will not follow the books' timelines accurately, and, the significance of events and memories will continue to be random.

### Usage example

```bash
go run cmd/book/main.go
```